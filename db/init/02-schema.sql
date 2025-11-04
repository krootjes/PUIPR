CREATE TABLE IF NOT EXISTS plex_users (
  id            BIGINT PRIMARY KEY,
  username      TEXT NOT NULL,
  friendly_name TEXT,
  user_thumb    TEXT,
  last_seen     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS user_ip_history (
  id         BIGSERIAL PRIMARY KEY,
  user_id    BIGINT NOT NULL REFERENCES plex_users(id) ON DELETE CASCADE,
  ip         INET   NOT NULL,
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

CREATE OR REPLACE FUNCTION get_user_ips(uid BIGINT)
RETURNS TABLE(ip INET, first_seen TIMESTAMPTZ, last_seen TIMESTAMPTZ)
LANGUAGE sql STABLE AS $$
  SELECT uih.ip, uih.first_seen, uih.last_seen
  FROM user_ip_history uih
  WHERE uih.user_id = uid
  ORDER BY last_seen DESC;
$$;

GRANT SELECT, INSERT, UPDATE, DELETE ON plex_users, user_ip_history TO puipr;
GRANT SELECT ON users_last_ip TO puipr;
GRANT EXECUTE ON FUNCTION get_user_ips(BIGINT) TO puipr;
