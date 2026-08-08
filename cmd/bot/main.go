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

	bot, err := newBotFromConfig(cfg, valorantbot.New)
	if err != nil {
		log.Fatalf("bot: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := bot.Run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}
}

func botConfig(cfg config.Config) valorantbot.Config {
	return valorantbot.Config{
		DiscordToken:       cfg.DiscordToken,
		DiscordAppID:       cfg.DiscordAppID,
		DiscordGuildID:     cfg.DiscordGuildID,
		BotSecret:          cfg.BotSecret,
		AuthPort:           cfg.AuthPort,
		AuthBindAddress:    cfg.AuthBindAddress,
		AuthBaseURL:        cfg.AuthBaseURL,
		DatabasePath:       cfg.DatabasePath,
		StoreResetCron:     cfg.StoreResetCron,
		CaptchaBrowserMode: cfg.CaptchaBrowserMode,
		CaptchaDisplay:     cfg.CaptchaDisplay,
	}
}

func newBotFromConfig(cfg config.Config, construct func(valorantbot.Config) (*valorantbot.Bot, error)) (*valorantbot.Bot, error) {
	return construct(botConfig(cfg))
}
