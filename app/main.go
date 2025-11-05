package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed templates/*
var tplFS embed.FS

type Server struct {
	db  *sql.DB
	tpl *template.Template
	// fetcher config
	tautulliURL    string
	tautulliAPIKey string
	tautulliLength int
	fetchInterval  time.Duration
}

type TautulliItem struct {
	UserID       int64   `json:"user_id"`
	User         string  `json:"user"`
	FriendlyName *string `json:"friendly_name"`
	UserThumb    *string `json:"user_thumb"`
	IPAddress    *string `json:"ip_address"`
	Date         *int64  `json:"date"`
}

func env(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

func mustFileDir(p string) {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", dir, err)
	}
}

func main() {
	dbPath := env("APP_DB_PATH", "/data/puipr.db")
	addr := env("APP_ADDR", "0.0.0.0:1707")

	mustFileDir(dbPath)

	// SQLite DSN met WAL + busy_timeout
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // SQLite: single-writer

	if err := pingRetry(db, 10, 300*time.Millisecond); err != nil {
		log.Fatalf("db ping failed: %v", err)
	}
	if err := ensureSchema(db); err != nil {
		log.Fatal(err)
	}

	// Custom tijdformatter: dd/mm/yyyy hh:mm
	formatTime := func(ts string) string {
		if ts == "" {
			return ""
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return ts
		}
		loc, _ := time.LoadLocation("Europe/Amsterdam") // lokale tijd
		return t.In(loc).Format("02/01/2006 15:04")
	}

	// Template met formatterfunctie
	tpl := template.Must(template.New("").Funcs(template.FuncMap{"formatTime": formatTime}).ParseFS(tplFS, "templates/*.html"))

	// Fetcher config uit env
	tURL := env("TAUTULLI_URL", "")
	tKey := env("TAUTULLI_APIKEY", "")
	lenStr := env("TAUTULLI_LENGTH", "100")
	intervalStr := env("FETCH_INTERVAL", "5m")
	tLen, _ := strconv.Atoi(lenStr)
	if tLen <= 0 {
		tLen = 100
	}
	itv, err := time.ParseDuration(intervalStr)
	if err != nil || itv < time.Second {
		itv = 5 * time.Minute
	}

	s := &Server{
		db:             db,
		tpl:            tpl,
		tautulliURL:    tURL,
		tautulliAPIKey: tKey,
		tautulliLength: tLen,
		fetchInterval:  itv,
	}

	// Start fetcher goroutine als geconfigureerd
	if s.tautulliURL != "" && s.tautulliAPIKey != "" {
		go s.runFetcher()
		log.Printf("Fetcher enabled: %s every %s (length=%d)", s.tautulliURL, s.fetchInterval, s.tautulliLength)
	} else {
		log.Printf("Fetcher disabled (set TAUTULLI_URL and TAUTULLI_APIKEY to enable)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/partial/summary", s.handleSummary)
	mux.HandleFunc("/partial/user/", s.handleUserIPs) // /partial/user/{id}
	mux.HandleFunc("/ingest", s.handleIngest)         // POST (array or {data:[...]})

	log.Printf("PUIPR (SQLite) listening on %s (db: %s)", addr, dbPath)
	log.Fatal(http.ListenAndServe(addr, logRequest(mux)))
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		loc, _ := time.LoadLocation("Europe/Amsterdam")
		log.Printf("[%s] %s %s (%v)", time.Now().In(loc).Format("02/01/2006 15:04"), r.Method, r.URL.Path, time.Since(start))
	})
}

func pingRetry(db *sql.DB, tries int, backoff time.Duration) error {
	var err error
	for i := 1; i <= tries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = db.PingContext(ctx)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(backoff)
	}
	return err
}

func ensureSchema(db *sql.DB) error {
	stmt := `
CREATE TABLE IF NOT EXISTS plex_users (
  id            INTEGER PRIMARY KEY,         -- user_id
  username      TEXT NOT NULL,
  friendly_name TEXT,
  user_thumb    TEXT,
  last_seen     TEXT NOT NULL                -- RFC3339
);

CREATE TABLE IF NOT EXISTS user_ip_history (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL,
  ip         TEXT NOT NULL,
  first_seen TEXT NOT NULL,
  last_seen  TEXT NOT NULL,
  CONSTRAINT uq_user_ip UNIQUE (user_id, ip),
  FOREIGN KEY(user_id) REFERENCES plex_users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_user_ip_history_user_last
ON user_ip_history (user_id, last_seen DESC);

CREATE VIEW IF NOT EXISTS users_last_ip AS
SELECT
  pu.id AS user_id,
  pu.username,
  pu.friendly_name,
  (
    SELECT uih.ip
    FROM user_ip_history uih
    WHERE uih.user_id = pu.id
    ORDER BY uih.last_seen DESC
    LIMIT 1
  ) AS last_ip,
  (
    SELECT uih.last_seen
    FROM user_ip_history uih
    WHERE uih.user_id = pu.id
    ORDER BY uih.last_seen DESC
    LIMIT 1
  ) AS updated_at
FROM plex_users pu;
`
	_, err := db.Exec(stmt)
	return err
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

	sqlq := `
SELECT pu.id, pu.username, pu.friendly_name, uli.last_ip, uli.updated_at
FROM users_last_ip uli
JOIN plex_users pu ON pu.id = uli.user_id
`
	args := []any{}
	if q != "" {
		sqlq += "WHERE pu.username LIKE ? OR IFNULL(uli.last_ip,'') LIKE ? OR IFNULL(pu.friendly_name,'') LIKE ?\n"
		p := "%" + q + "%"
		args = append(args, p, p, p)
	}
	sqlq += "ORDER BY pu.username ASC"

	type Row struct {
		UserID       int64
		Username     string
		FriendlyName *string
		LastIP       *string
		UpdatedAt    *string
	}

	rows, err := s.db.Query(sqlq, args...)
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
	_ = s.db.QueryRow(`SELECT username FROM plex_users WHERE id = ?`, uid).Scan(&username)
	if username == "" {
		username = fmt.Sprintf("User %d", uid)
	}

	type IPRow struct {
		IP        string
		FirstSeen string
		LastSeen  string
	}
	rows, err := s.db.Query(`
SELECT ip, first_seen, last_seen
FROM user_ip_history
WHERE user_id = ?
ORDER BY last_seen DESC
`, uid)
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
	// Optionele bearer token
	want := os.Getenv("INGEST_TOKEN")
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if want != "" && got != want {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	defer r.Body.Close()

	var arr []TautulliItem
	if err := json.Unmarshal(body, &arr); err != nil {
		var obj struct {
			Data []TautulliItem `json:"data"`
		}
		if e2 := json.Unmarshal(body, &obj); e2 != nil {
			http.Error(w, "bad json", 400)
			return
		}
		arr = obj.Data
	}
	if len(arr) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ingested":0}`))
		return
	}

	if err := s.ingestItems(r.Context(), arr); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(fmt.Sprintf(`{"ingested":%d}`, len(arr))))
}

func (s *Server) ingestItems(ctx context.Context, arr []TautulliItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, it := range arr {
		ts := time.Now().UTC()
		if it.Date != nil && *it.Date > 0 {
			ts = time.Unix(*it.Date, 0).UTC()
		}
		tsStr := ts.Format(time.RFC3339)

		// Upsert user
		if _, err := tx.Exec(`
INSERT INTO plex_users (id, username, friendly_name, user_thumb, last_seen)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  username=excluded.username,
  friendly_name=excluded.friendly_name,
  user_thumb=excluded.user_thumb,
  last_seen=CASE WHEN plex_users.last_seen > excluded.last_seen THEN plex_users.last_seen ELSE excluded.last_seen END
`, it.UserID, it.User, it.FriendlyName, it.UserThumb, tsStr); err != nil {
			return fmt.Errorf("user upsert: %w", err)
		}

		// Upsert IP
		if it.IPAddress != nil && *it.IPAddress != "" {
			if _, err := tx.Exec(`
INSERT INTO user_ip_history (user_id, ip, first_seen, last_seen)
VALUES (?, ?, ?, ?)
ON CONFLICT(user_id, ip) DO UPDATE SET
  last_seen=CASE WHEN user_ip_history.last_seen > excluded.last_seen THEN user_ip_history.last_seen ELSE excluded.last_seen END
`, it.UserID, *it.IPAddress, tsStr, tsStr); err != nil {
				return fmt.Errorf("ip upsert: %w", err)
			}
		}
	}

	return tx.Commit()
}

// ---------------- Fetcher (in-app) ----------------

func (s *Server) runFetcher() {
	client := &http.Client{Timeout: 15 * time.Second}

	// run immediately, then on ticker
	s.fetchOnce(client)
	t := time.NewTicker(s.fetchInterval)
	defer t.Stop()
	for range t.C {
		s.fetchOnce(client)
	}
}

func (s *Server) fetchOnce(client *http.Client) {
	items, err := s.fetchTautulli(client)
	if err != nil {
		log.Printf("fetch error: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.ingestItems(ctx, items); err != nil {
		log.Printf("ingest error: %v", err)
		return
	}
	log.Printf("ingested %d items from Tautulli: %s", len(items), s.tautulliURL)
}

func (s *Server) fetchTautulli(client *http.Client) ([]TautulliItem, error) {
	u, err := url.Parse(s.tautulliURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("apikey", s.tautulliAPIKey)
	q.Set("cmd", "get_history")
	q.Set("length", strconv.Itoa(s.tautulliLength))
	q.Set("order_column", "date")
	q.Set("order_dir", "desc")
	u.RawQuery = q.Encode()

	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("tautulli http %d: %s", resp.StatusCode, string(body))
	}

	var outer struct {
		Response struct {
			Result string `json:"result"`
			Data   struct {
				Data []TautulliItem `json:"data"`
			} `json:"data"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if strings.ToLower(outer.Response.Result) != "success" {
		return nil, fmt.Errorf("tautulli result: %s", outer.Response.Result)
	}
	return outer.Response.Data.Data, nil
}
