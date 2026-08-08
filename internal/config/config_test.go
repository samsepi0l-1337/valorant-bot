package config

import (
	"os"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"DISCORD_TOKEN",
		"DISCORD_APP_ID",
		"DISCORD_GUILD_ID",
		"BOT_SECRET",
		"AUTH_PORT",
		"AUTH_BASE_URL",
		"DATABASE_PATH",
		"STORE_RESET_CRON",
		"CAPTCHA_BROWSER_MODE",
		"CAPTCHA_DISPLAY",
	}
	for _, k := range keys {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DISCORD_TOKEN", "test-token")
	t.Setenv("DISCORD_APP_ID", "app-123")
	t.Setenv("BOT_SECRET", "0123456789abcdef0123456789abcdef") // 32 chars
	t.Setenv("AUTH_BASE_URL", "http://192.168.0.50:8787")
}

func TestLoad_MissingRequired(t *testing.T) {
	clearEnv(t)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when required env vars missing")
	}
}

func TestLoad_MissingDiscordToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("DISCORD_APP_ID", "app-123")
	t.Setenv("BOT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("AUTH_BASE_URL", "http://192.168.0.50:8787")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when DISCORD_TOKEN missing")
	}
}

func TestLoad_MissingBotSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv("DISCORD_TOKEN", "tok")
	t.Setenv("DISCORD_APP_ID", "app-123")
	t.Setenv("AUTH_BASE_URL", "http://192.168.0.50:8787")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when BOT_SECRET missing")
	}
}

func TestLoad_BotSecretTooShort(t *testing.T) {
	clearEnv(t)
	t.Setenv("DISCORD_TOKEN", "tok")
	t.Setenv("DISCORD_APP_ID", "app-123")
	t.Setenv("BOT_SECRET", "short")
	t.Setenv("AUTH_BASE_URL", "http://192.168.0.50:8787")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when BOT_SECRET shorter than 32 chars")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DiscordToken != "test-token" {
		t.Errorf("DiscordToken = %q", cfg.DiscordToken)
	}
	if cfg.DiscordAppID != "app-123" {
		t.Errorf("DiscordAppID = %q", cfg.DiscordAppID)
	}
	if cfg.DiscordGuildID != "" {
		t.Errorf("DiscordGuildID should be empty, got %q", cfg.DiscordGuildID)
	}
	if cfg.BotSecret != "0123456789abcdef0123456789abcdef" {
		t.Errorf("BotSecret = %q", cfg.BotSecret)
	}
	if cfg.AuthPort != 8787 {
		t.Errorf("AuthPort = %d, want 8787", cfg.AuthPort)
	}
	if cfg.AuthBaseURL != "http://192.168.0.50:8787" {
		t.Errorf("AuthBaseURL = %q", cfg.AuthBaseURL)
	}
	if cfg.DatabasePath != "./data/bot.db" {
		t.Errorf("DatabasePath = %q, want ./data/bot.db", cfg.DatabasePath)
	}
	if cfg.StoreResetCron != "0 0 * * *" {
		t.Errorf("StoreResetCron = %q, want 0 0 * * *", cfg.StoreResetCron)
	}
	if cfg.CaptchaBrowserMode != "local" {
		t.Errorf("CaptchaBrowserMode = %q, want local", cfg.CaptchaBrowserMode)
	}
	if cfg.CaptchaDisplay != ":99" {
		t.Errorf("CaptchaDisplay = %q, want :99", cfg.CaptchaDisplay)
	}
}

func TestLoad_Overrides(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("DISCORD_GUILD_ID", "guild-99")
	t.Setenv("AUTH_PORT", "9999")
	t.Setenv("DATABASE_PATH", "/tmp/custom.db")
	t.Setenv("STORE_RESET_CRON", "0 8 * * *")
	t.Setenv("CAPTCHA_BROWSER_MODE", "REMOTE")
	t.Setenv("CAPTCHA_DISPLAY", ":42")
	t.Setenv("AUTH_BASE_URL", "https://relay.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DiscordGuildID != "guild-99" {
		t.Errorf("DiscordGuildID = %q", cfg.DiscordGuildID)
	}
	if cfg.AuthPort != 9999 {
		t.Errorf("AuthPort = %d, want 9999", cfg.AuthPort)
	}
	if cfg.DatabasePath != "/tmp/custom.db" {
		t.Errorf("DatabasePath = %q", cfg.DatabasePath)
	}
	if cfg.StoreResetCron != "0 8 * * *" {
		t.Errorf("StoreResetCron = %q", cfg.StoreResetCron)
	}
	if cfg.CaptchaBrowserMode != "remote" {
		t.Errorf("CaptchaBrowserMode = %q, want remote", cfg.CaptchaBrowserMode)
	}
	if cfg.CaptchaDisplay != ":42" {
		t.Errorf("CaptchaDisplay = %q, want :42", cfg.CaptchaDisplay)
	}
}

func TestLoad_RejectsInvalidRemoteCaptchaOrigin(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("CAPTCHA_BROWSER_MODE", "remote")
	t.Setenv("AUTH_BASE_URL", "http://relay.example.com")

	_, err := Load()
	if err == nil {
		t.Fatal("expected invalid remote CAPTCHA origin error")
	}
}

func TestLoad_InvalidAuthPort(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("AUTH_PORT", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid AUTH_PORT")
	}
}
