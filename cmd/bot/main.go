package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/dosfsociety/valorant-bot/internal/config"
	"github.com/dosfsociety/valorant-bot/pkg/valorantbot"
	"github.com/joho/godotenv"
)

func main() {
	// Load .env if present (local / Pi); existing OS env wins.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	bot, err := valorantbot.New(valorantbot.Config{
		DiscordToken:       cfg.DiscordToken,
		DiscordAppID:       cfg.DiscordAppID,
		DiscordGuildID:     cfg.DiscordGuildID,
		BotSecret:          cfg.BotSecret,
		AuthPort:           cfg.AuthPort,
		AuthBaseURL:        cfg.AuthBaseURL,
		DatabasePath:       cfg.DatabasePath,
		StoreResetCron:     cfg.StoreResetCron,
		CaptchaBrowserMode: cfg.CaptchaBrowserMode,
		CaptchaDisplay:     cfg.CaptchaDisplay,
	})
	if err != nil {
		log.Fatalf("bot: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := bot.Run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}
}
