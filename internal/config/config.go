package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
)

// Config holds runtime settings loaded from the environment.
type Config struct {
	DiscordToken       string
	DiscordAppID       string
	DiscordGuildID     string
	BotSecret          string
	AuthPort           int
	AuthBaseURL        string
	DatabasePath       string
	StoreResetCron     string
	CaptchaBrowserMode string
	CaptchaDisplay     string
}

// Load reads configuration from environment variables.
// Required: DISCORD_TOKEN, DISCORD_APP_ID, BOT_SECRET (>=32 chars), AUTH_BASE_URL.
func Load() (Config, error) {
	cfg := Config{
		DiscordToken:       os.Getenv("DISCORD_TOKEN"),
		DiscordAppID:       os.Getenv("DISCORD_APP_ID"),
		DiscordGuildID:     os.Getenv("DISCORD_GUILD_ID"),
		BotSecret:          os.Getenv("BOT_SECRET"),
		AuthBaseURL:        os.Getenv("AUTH_BASE_URL"),
		DatabasePath:       os.Getenv("DATABASE_PATH"),
		StoreResetCron:     os.Getenv("STORE_RESET_CRON"),
		CaptchaBrowserMode: os.Getenv("CAPTCHA_BROWSER_MODE"),
		CaptchaDisplay:     os.Getenv("CAPTCHA_DISPLAY"),
		AuthPort:           8787,
	}

	if cfg.DiscordToken == "" {
		return Config{}, fmt.Errorf("DISCORD_TOKEN is required")
	}
	if cfg.DiscordAppID == "" {
		return Config{}, fmt.Errorf("DISCORD_APP_ID is required")
	}
	if cfg.BotSecret == "" {
		return Config{}, fmt.Errorf("BOT_SECRET is required")
	}
	if len(cfg.BotSecret) < 32 {
		return Config{}, fmt.Errorf("BOT_SECRET must be at least 32 characters")
	}
	if cfg.AuthBaseURL == "" {
		return Config{}, fmt.Errorf("AUTH_BASE_URL is required")
	}
	mode, err := netutil.NormalizeCaptchaBrowserMode(cfg.CaptchaBrowserMode, cfg.AuthBaseURL)
	if err != nil {
		return Config{}, err
	}
	cfg.CaptchaBrowserMode = string(mode)

	if portStr := os.Getenv("AUTH_PORT"); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return Config{}, fmt.Errorf("AUTH_PORT: %w", err)
		}
		cfg.AuthPort = port
	}

	if cfg.DatabasePath == "" {
		cfg.DatabasePath = "./data/bot.db"
	}
	if cfg.StoreResetCron == "" {
		cfg.StoreResetCron = "0 0 * * *"
	}
	if cfg.CaptchaDisplay == "" {
		cfg.CaptchaDisplay = ":99"
	}

	return cfg, nil
}
