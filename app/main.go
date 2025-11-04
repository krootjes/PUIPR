package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/*
var tplFS embed.FS

type Server struct {
	db  *pgxpool.Pool
	tpl *template.Template
}

type TautulliItem struct {
	UserID       int64   `json:"user_id"`
	User         string  `json:"user"`
	FriendlyName *string `json:"friendly_name"`
	UserThumb    *string `json:"user_thumb"`
	IPAddress    *string `json:"ip_address"`
	Date         *int64  `json:"date"`
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing env %s", k)
	}
	return v
}

func main() {
	dsn := mustEnv("DATABASE_URL")
	addr := os.Getenv("APP_ADDR")
	if addr == "" {
		addr = "0.0.0.0:8080"
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Fatal(err)
	}
	db, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	tpl := template.Must(template.ParseFS(tplFS, "templates/*.html"))
	s := &Server{db: db, tpl: tpl}

	if err := s.ensureSchema(context.Background()); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/partial/summary", s.handleSummary)
	mux.HandleFunc("/partial/user/", s.handleUserIPs) // /partial/user/{id}
	mux.HandleFunc("/ingest", s.handleIngest)         // POST (array or {data:[...]})

	log.Printf("PUIPR listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, logRequest(mux)))
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %v", r.Method, r.URL.Path, time.Since(start))
	})
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	_ = s.tpl.ExecuteTemplate(w, "index.html", nil)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	sql := `
SELECT pu.id AS user_id, pu.username, pu.friendly_name, uli.last_ip::text, uli.updated_at
FROM users_last_ip uli
JOIN plex_users pu ON pu.id = uli.user_id
`
	if q != "" {
		esc := strings.ReplaceAll(q, "'", "''")
		sql += "WHERE pu.username ILIKE '%" + esc + "%' OR COALESCE(uli.last_ip::text,'') ILIKE '%" + esc + "%' OR COALESCE(pu.friendly_name,'') ILIKE '%" + esc + "%'\n"
	}
	sql += "ORDER BY pu.username ASC"

	type Row struct {
		UserID       int64
		Username     string
		FriendlyName *string
		LastIP       *string
		UpdatedAt    *time.Time
	}
	rows, err := s.db.Query(r.Context(), sql)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var list []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.UserID, &r.Username, &r.FriendlyName, &r.LastIP, &r.UpdatedAt); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		list = append(list, r)
	}
	_ = s.tpl.ExecuteTemplate(w, "summary.html", map[string]any{"Rows": list})
}

func (s *Server) handleUserIPs(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/partial/user/")
	uid, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}

	var username string
	_ = s.db.QueryRow(r.Context(), `SELECT username FROM plex_users WHERE id=$1`, uid).Scan(&username)
	if username == "" {
		username = fmt.Sprintf("User %d", uid)
	}

	type IPRow struct {
		IP                  string
		FirstSeen, LastSeen time.Time
	}
	rows, err := s.db.Query(r.Context(), `
SELECT ip::text, first_seen, last_seen
FROM user_ip_history
WHERE user_id=$1
ORDER BY last_seen DESC`, uid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var list []IPRow
	for rows.Next() {
		var x IPRow
		if err := rows.Scan(&x.IP, &x.FirstSeen, &x.LastSeen); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		list = append(list, x)
	}
	_ = s.tpl.ExecuteTemplate(w, "user_ips.html", map[string]any{"Username": username, "IPs": list})
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	defer r.Body.Close()

	// array OR {data:[...]}
	var arr []TautulliItem
	if err := json.Unmarshal(body, &arr); err != nil {
		var obj struct {
			Data []TautulliItem `json:"data"`
		}
		_ = json.Unmarshal(body, &obj)
		arr = obj.Data
	}
	if len(arr) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ingested":0}`))
		return
	}

	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()
	for _, it := range arr {
		ts := now
		if it.Date != nil && *it.Date > 0 {
			ts = time.Unix(*it.Date, 0).UTC()
		}

		_, err = tx.Exec(ctx, `
INSERT INTO plex_users(id, username, friendly_name, user_thumb, last_seen)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (id) DO UPDATE
SET username=EXCLUDED.username,
    friendly_name=EXCLUDED.friendly_name,
    user_thumb=EXCLUDED.user_thumb,
    last_seen=GREATEST(plex_users.last_seen, EXCLUDED.last_seen)
`, it.UserID, it.User, it.FriendlyName, it.UserThumb, ts)
		if err != nil {
			http.Error(w, "user upsert: "+err.Error(), 500)
			return
		}

		if it.IPAddress != nil && *it.IPAddress != "" {
			_, err = tx.Exec(ctx, `
INSERT INTO user_ip_history(user_id, ip, first_seen, last_seen)
VALUES ($1,$2,$3,$3)
ON CONFLICT (user_id, ip) DO UPDATE
SET last_seen = GREATEST(user_ip_history.last_seen, EXCLUDED.last_seen)
`, it.UserID, *it.IPAddress, ts)
			if err != nil {
				http.Error(w, "ip upsert: "+err.Error(), 500)
				return
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(fmt.Sprintf(`{"ingested":%d}`, len(arr))))
}

func (s *Server) ensureSchema(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `
CREATE TABLE IF NOT EXISTS plex_users (
  id BIGINT PRIMARY KEY,
  username TEXT NOT NULL,
  friendly_name TEXT,
  user_thumb TEXT,
  last_seen TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS user_ip_history (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES plex_users(id) ON DELETE CASCADE,
  ip INET NOT NULL,
  first_seen TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT uq_user_ip UNIQUE (user_id, ip)
);
CREATE OR REPLACE VIEW users_last_ip AS
SELECT pu.id AS user_id,
       pu.username,
       pu.friendly_name,
       (SELECT uih.ip FROM user_ip_history uih
         WHERE uih.user_id = pu.id
         ORDER BY uih.last_seen DESC
         LIMIT 1) AS last_ip,
       (SELECT uih.last_seen FROM user_ip_history uih
         WHERE uih.user_id = pu.id
         ORDER BY uih.last_seen DESC
         LIMIT 1) AS updated_at
FROM plex_users pu;
`)
	return err
}
