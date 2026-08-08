package valorantbot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestNewNormalizesCaptchaBrowserConfig(t *testing.T) {
	bot, err := New(Config{
		DiscordToken:       "token",
		DiscordAppID:       "app-id",
		BotSecret:          "0123456789abcdef0123456789abcdef",
		AuthBaseURL:        "https://relay.example.com",
		CaptchaBrowserMode: " ReMoTe ",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if bot.cfg.CaptchaBrowserMode != "remote" {
		t.Fatalf("CaptchaBrowserMode = %q, want remote", bot.cfg.CaptchaBrowserMode)
	}
	if bot.cfg.CaptchaDisplay != ":99" {
		t.Fatalf("CaptchaDisplay = %q, want :99", bot.cfg.CaptchaDisplay)
	}
}

func TestNewRejectsInvalidRemoteCaptchaOrigin(t *testing.T) {
	_, err := New(Config{
		DiscordToken:       "token",
		DiscordAppID:       "app-id",
		BotSecret:          "0123456789abcdef0123456789abcdef",
		AuthBaseURL:        "http://relay.example.com",
		CaptchaBrowserMode: "remote",
	})
	if err == nil {
		t.Fatal("expected invalid remote CAPTCHA origin error")
	}
}

func TestTrackedSchedulerSupportsBoundedDrainThenBackgroundJoin(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	task := startTrackedScheduler(func() error {
		close(started)
		<-release
		return nil
	})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		unblock()
		t.Fatal("scheduler task did not start")
	}

	bounded, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := task.wait(bounded); !errors.Is(err, context.DeadlineExceeded) {
		unblock()
		t.Fatalf("bounded wait error = %v, want deadline exceeded", err)
	}

	dependencyClosed := make(chan struct{})
	go closeRuntimeAfterScheduler(task, func() { close(dependencyClosed) })
	select {
	case <-dependencyClosed:
		unblock()
		t.Fatal("dependency closed before scheduler task completed")
	case <-time.After(50 * time.Millisecond):
	}

	unblock()
	select {
	case <-dependencyClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("background join did not complete after scheduler task returned")
	}
}
