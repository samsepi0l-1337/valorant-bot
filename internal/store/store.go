package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed persistence layer.
type Store struct {
	db *sql.DB
}

// Account is a linked Riot account for a Discord user.
type Account struct {
	ID                int64
	DiscordUserID     string
	PUUID             string
	GameName          string
	TagLine           string
	Region            string
	Shard             string
	CookiesCiphertext []byte
	CreatedAt         time.Time
}

// WishlistItem is a skin on a user's wishlist.
type WishlistItem struct {
	DiscordUserID string
	SkinUUID      string
	SkinName      string
}

// GuildSettings holds per-guild bot settings.
type GuildSettings struct {
	GuildID        string
	DailyChannelID string
	Enabled        bool
}

// Open opens (or creates) a SQLite database at path and migrates the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	// busy_timeout + WAL avoid SQLITE_BUSY under concurrent Discord interactions.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", filepath.ToSlash(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS riot_accounts (
	id INTEGER PRIMARY KEY,
	discord_user_id TEXT NOT NULL,
	puuid TEXT NOT NULL UNIQUE,
	game_name TEXT,
	tag_line TEXT,
	region TEXT,
	shard TEXT,
	cookies_ciphertext BLOB NOT NULL,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS auth_pending (
	state TEXT PRIMARY KEY,
	discord_user_id TEXT NOT NULL,
	expires_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS wishlists (
	discord_user_id TEXT NOT NULL,
	skin_uuid TEXT NOT NULL,
	skin_name TEXT,
	PRIMARY KEY (discord_user_id, skin_uuid)
);
CREATE TABLE IF NOT EXISTS guild_settings (
	guild_id TEXT PRIMARY KEY,
	daily_channel_id TEXT,
	enabled INTEGER DEFAULT 1
);
CREATE TABLE IF NOT EXISTS user_settings (
	discord_user_id TEXT PRIMARY KEY,
	language TEXT NOT NULL DEFAULT 'ko'
);
`
	_, err := s.db.Exec(schema)
	return err
}

// UpsertRiotAccount inserts or updates a Riot account by puuid.
func (s *Store) UpsertRiotAccount(a Account) error {
	_, err := s.db.Exec(`
INSERT INTO riot_accounts (discord_user_id, puuid, game_name, tag_line, region, shard, cookies_ciphertext)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(puuid) DO UPDATE SET
	discord_user_id = excluded.discord_user_id,
	game_name = excluded.game_name,
	tag_line = excluded.tag_line,
	region = excluded.region,
	shard = excluded.shard,
	cookies_ciphertext = excluded.cookies_ciphertext
`, a.DiscordUserID, a.PUUID, a.GameName, a.TagLine, a.Region, a.Shard, a.CookiesCiphertext)
	return err
}

// ListRiotAccountsByDiscord returns all Riot accounts for a Discord user.
func (s *Store) ListRiotAccountsByDiscord(discordUserID string) ([]Account, error) {
	rows, err := s.db.Query(`
SELECT id, discord_user_id, puuid, COALESCE(game_name,''), COALESCE(tag_line,''),
       COALESCE(region,''), COALESCE(shard,''), cookies_ciphertext, created_at
FROM riot_accounts WHERE discord_user_id = ?
`, discordUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var a Account
		var created string
		if err := rows.Scan(&a.ID, &a.DiscordUserID, &a.PUUID, &a.GameName, &a.TagLine,
			&a.Region, &a.Shard, &a.CookiesCiphertext, &created); err != nil {
			return nil, err
		}
		if t, err := time.Parse("2006-01-02 15:04:05", created); err == nil {
			a.CreatedAt = t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAllRiotAccounts returns every linked Riot account.
func (s *Store) ListAllRiotAccounts() ([]Account, error) {
	rows, err := s.db.Query(`
SELECT id, discord_user_id, puuid, COALESCE(game_name,''), COALESCE(tag_line,''),
       COALESCE(region,''), COALESCE(shard,''), cookies_ciphertext, created_at
FROM riot_accounts
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var a Account
		var created string
		if err := rows.Scan(&a.ID, &a.DiscordUserID, &a.PUUID, &a.GameName, &a.TagLine,
			&a.Region, &a.Shard, &a.CookiesCiphertext, &created); err != nil {
			return nil, err
		}
		if t, err := time.Parse("2006-01-02 15:04:05", created); err == nil {
			a.CreatedAt = t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteRiotAccount removes a Riot account for the given Discord user and puuid.
func (s *Store) DeleteRiotAccount(discordUserID, puuid string) error {
	_, err := s.db.Exec(`DELETE FROM riot_accounts WHERE discord_user_id = ? AND puuid = ?`, discordUserID, puuid)
	return err
}

// PutAuthPending stores a pending auth state.
func (s *Store) PutAuthPending(state, discordUserID string, expiresAt time.Time) error {
	_, err := s.db.Exec(`
INSERT INTO auth_pending (state, discord_user_id, expires_at) VALUES (?, ?, ?)
ON CONFLICT(state) DO UPDATE SET
	discord_user_id = excluded.discord_user_id,
	expires_at = excluded.expires_at
`, state, discordUserID, expiresAt.UTC().Format("2006-01-02 15:04:05"))
	return err
}

// TakeAuthPending deletes and returns the Discord user for a pending auth state.
func (s *Store) TakeAuthPending(state string) (discordUserID string, ok bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback() }()

	err = tx.QueryRow(`SELECT discord_user_id FROM auth_pending WHERE state = ?`, state).Scan(&discordUserID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(`DELETE FROM auth_pending WHERE state = ?`, state); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return discordUserID, true, nil
}

// AddWishlist adds a skin to a user's wishlist (idempotent).
func (s *Store) AddWishlist(discordUserID, skinUUID, skinName string) error {
	_, err := s.db.Exec(`
INSERT INTO wishlists (discord_user_id, skin_uuid, skin_name) VALUES (?, ?, ?)
ON CONFLICT(discord_user_id, skin_uuid) DO UPDATE SET skin_name = excluded.skin_name
`, discordUserID, skinUUID, skinName)
	return err
}

// RemoveWishlist removes a skin from a user's wishlist.
func (s *Store) RemoveWishlist(discordUserID, skinUUID string) error {
	_, err := s.db.Exec(`DELETE FROM wishlists WHERE discord_user_id = ? AND skin_uuid = ?`, discordUserID, skinUUID)
	return err
}

// ListWishlists returns wishlist items for a Discord user.
func (s *Store) ListWishlists(discordUserID string) ([]WishlistItem, error) {
	rows, err := s.db.Query(`
SELECT discord_user_id, skin_uuid, COALESCE(skin_name,'') FROM wishlists WHERE discord_user_id = ?
`, discordUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WishlistItem
	for rows.Next() {
		var w WishlistItem
		if err := rows.Scan(&w.DiscordUserID, &w.SkinUUID, &w.SkinName); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListAllWishlists returns every wishlist item across all users.
func (s *Store) ListAllWishlists() ([]WishlistItem, error) {
	rows, err := s.db.Query(`
SELECT discord_user_id, skin_uuid, COALESCE(skin_name,'') FROM wishlists
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WishlistItem
	for rows.Next() {
		var w WishlistItem
		if err := rows.Scan(&w.DiscordUserID, &w.SkinUUID, &w.SkinName); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpsertGuildSettings inserts or updates guild settings.
func (s *Store) UpsertGuildSettings(gs GuildSettings) error {
	enabled := 0
	if gs.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(`
INSERT INTO guild_settings (guild_id, daily_channel_id, enabled) VALUES (?, ?, ?)
ON CONFLICT(guild_id) DO UPDATE SET
	daily_channel_id = excluded.daily_channel_id,
	enabled = excluded.enabled
`, gs.GuildID, gs.DailyChannelID, enabled)
	return err
}

// GetGuildSettings returns settings for a guild.
func (s *Store) GetGuildSettings(guildID string) (GuildSettings, bool, error) {
	var gs GuildSettings
	var enabled int
	err := s.db.QueryRow(`
SELECT guild_id, COALESCE(daily_channel_id,''), enabled FROM guild_settings WHERE guild_id = ?
`, guildID).Scan(&gs.GuildID, &gs.DailyChannelID, &enabled)
	if err == sql.ErrNoRows {
		return GuildSettings{}, false, nil
	}
	if err != nil {
		return GuildSettings{}, false, err
	}
	gs.Enabled = enabled != 0
	return gs, true, nil
}

// ListEnabledGuildSettings returns all guilds with daily posts enabled.
func (s *Store) ListEnabledGuildSettings() ([]GuildSettings, error) {
	rows, err := s.db.Query(`
SELECT guild_id, COALESCE(daily_channel_id,''), enabled FROM guild_settings WHERE enabled = 1
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GuildSettings
	for rows.Next() {
		var gs GuildSettings
		var enabled int
		if err := rows.Scan(&gs.GuildID, &gs.DailyChannelID, &enabled); err != nil {
			return nil, err
		}
		gs.Enabled = enabled != 0
		out = append(out, gs)
	}
	return out, rows.Err()
}

// GetUserLanguage returns the user's UI language (default "ko").
func (s *Store) GetUserLanguage(discordUserID string) (string, error) {
	var lang string
	err := s.db.QueryRow(`SELECT language FROM user_settings WHERE discord_user_id = ?`, discordUserID).Scan(&lang)
	if err == sql.ErrNoRows {
		return "ko", nil
	}
	if err != nil {
		return "ko", err
	}
	if lang == "" {
		return "ko", nil
	}
	return lang, nil
}

// SetUserLanguage upserts the user's UI language.
func (s *Store) SetUserLanguage(discordUserID, language string) error {
	_, err := s.db.Exec(`
INSERT INTO user_settings (discord_user_id, language) VALUES (?, ?)
ON CONFLICT(discord_user_id) DO UPDATE SET language = excluded.language
`, discordUserID, language)
	return err
}
