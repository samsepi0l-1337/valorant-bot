package authweb

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
)

func TestNewDefaultsCaptchaBrowserModeAndDisplay(t *testing.T) {
	s := New(Deps{})
	if s.captchaBrowserMode != netutil.CaptchaBrowserLocal {
		t.Fatalf("captchaBrowserMode = %q, want local", s.captchaBrowserMode)
	}
	if s.captchaDisplay != ":99" {
		t.Fatalf("captchaDisplay = %q, want :99", s.captchaDisplay)
	}
}

func TestNewStoresRemoteCaptchaBrowserConfig(t *testing.T) {
	s := New(Deps{
		CaptchaBrowserMode: netutil.CaptchaBrowserRemote,
		CaptchaDisplay:     ":42",
	})
	if s.captchaBrowserMode != netutil.CaptchaBrowserRemote {
		t.Fatalf("captchaBrowserMode = %q, want remote", s.captchaBrowserMode)
	}
	if s.captchaDisplay != ":42" {
		t.Fatalf("captchaDisplay = %q, want :42", s.captchaDisplay)
	}
}

func TestRemoteCaptchaLaunchPassesConfiguredDisplay(t *testing.T) {
	pw := &fakePasswordAuth{}
	s := New(Deps{
		AuthBaseURL:        "https://relay.example.com",
		CaptchaBrowserMode: netutil.CaptchaBrowserRemote,
		CaptchaDisplay:     ":42",
		PasswordAuth:       pw,
		PendingTTL:         time.Minute,
	})
	t.Cleanup(func() { _ = s.Close() })

	var gotDisplay string
	s.launchRemoteCaptchaBrowser = func(_ string, display string) (captchaBrowserController, error) {
		gotDisplay = display
		return newTestCaptchaBrowserController(), nil
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "riot-user", "password")
	if err != nil {
		t.Fatalf("BeginPasswordLogin: %v", err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatalf("LaunchPasswordCaptcha: %v", err)
	}
	if gotDisplay != ":42" {
		t.Fatalf("remote CAPTCHA display = %q, want :42", gotDisplay)
	}
}

func TestDisabledCaptchaBrowserRejectsPasswordLaunchAndKeepsQRAvailable(t *testing.T) {
	qr := &mockQRAuth{pollsUntilDone: 1}
	pw := &fakeBrowserPasswordAuth{
		fakePasswordAuth: &fakePasswordAuth{},
		authorizeURL:     "https://auth.riotgames.com/authorize",
	}
	s := New(Deps{
		AuthBaseURL:        "https://relay.example.com",
		CaptchaBrowserMode: netutil.CaptchaBrowserDisabled,
		PasswordAuth:       pw,
		QRAuth:             qr,
		Store:              newMockStore(),
		PendingTTL:         time.Minute,
	})
	t.Cleanup(func() { _ = s.Close() })

	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "riot-user", "password")
	if err != nil {
		t.Fatalf("BeginPasswordLogin: %v", err)
	}
	err = s.LaunchPasswordCaptcha(context.Background(), state, "owner-1")
	if err == nil || !strings.Contains(err.Error(), "disabled") || !strings.Contains(err.Error(), "Riot Mobile QR") {
		t.Fatalf("LaunchPasswordCaptcha disabled error = %v", err)
	}

	if _, _, err := s.BeginQRAuth(context.Background(), "owner-1"); err != nil {
		t.Fatalf("BeginQRAuth while CAPTCHA disabled: %v", err)
	}
}
