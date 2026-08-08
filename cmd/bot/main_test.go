package main

import (
	"testing"

	"github.com/dosfsociety/valorant-bot/internal/config"
	"github.com/dosfsociety/valorant-bot/pkg/valorantbot"
)

func TestNewBotFromConfigPreservesCaptchaBrowserFields(t *testing.T) {
	var got valorantbot.Config
	_, err := newBotFromConfig(config.Config{
		CaptchaBrowserMode: "remote",
		CaptchaDisplay:     ":42",
		AuthBindAddress:    "::1",
	}, func(cfg valorantbot.Config) (*valorantbot.Bot, error) {
		got = cfg
		return &valorantbot.Bot{}, nil
	})
	if err != nil {
		t.Fatalf("newBotFromConfig: %v", err)
	}
	if got.CaptchaBrowserMode != "remote" {
		t.Fatalf("CaptchaBrowserMode = %q, want remote", got.CaptchaBrowserMode)
	}
	if got.CaptchaDisplay != ":42" {
		t.Fatalf("CaptchaDisplay = %q, want :42", got.CaptchaDisplay)
	}
	if got.AuthBindAddress != "::1" {
		t.Fatalf("AuthBindAddress = %q, want ::1", got.AuthBindAddress)
	}
}
