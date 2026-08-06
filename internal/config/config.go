package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds runtime settings loaded from the environment.
type Config struct {
	DiscordToken    string
	DiscordAppID    string
	DiscordGuildID  string
	BotSecret       string
	AuthPort        int
	AuthBaseURL     string
	DatabasePath    string
	StoreResetCron  string
}

// Load reads configuration from environment variables.
// Required: DISCORD_TOKEN, DISCORD_APP_ID, BOT_SECRET (>=32 chars), AUTH_BASE_URL.
func Load() (Config, error) {
	cfg := Config{
		DiscordToken:   os.Getenv("DISCORD_TOKEN"),
		DiscordAppID:   os.Getenv("DISCORD_APP_ID"),
		DiscordGuildID: os.Getenv("DISCORD_GUILD_ID"),
		BotSecret:      os.Getenv("BOT_SECRET"),
		AuthBaseURL:    os.Getenv("AUTH_BASE_URL"),
		DatabasePath:   os.Getenv("DATABASE_PATH"),
		StoreResetCron: os.Getenv("STORE_RESET_CRON"),
		AuthPort:       8787,
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

	return cfg, nil
}
