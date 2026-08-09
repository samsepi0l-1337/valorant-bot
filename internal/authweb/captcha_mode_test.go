package authweb

import (
	"context"
	"errors"
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

func TestPasswordLoginEnabledReflectsCaptchaBrowserMode(t *testing.T) {
	tests := []struct {
		name string
		mode netutil.CaptchaBrowserMode
		want bool
	}{
		{name: "default local", want: true},
		{name: "local", mode: netutil.CaptchaBrowserLocal, want: true},
		{name: "remote", mode: netutil.CaptchaBrowserRemote, want: true},
		{name: "disabled", mode: netutil.CaptchaBrowserDisabled, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := New(Deps{CaptchaBrowserMode: test.mode})
			t.Cleanup(func() { _ = s.Close() })
			if got := s.PasswordLoginEnabled(); got != test.want {
				t.Fatalf("PasswordLoginEnabled()=%v, want %v", got, test.want)
			}
		})
	}
}

func TestRemoteCaptchaLaunchWaitsForAuthenticatedViewer(t *testing.T) {
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
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err == nil ||
		!strings.Contains(err.Error(), "authenticated viewer") {
		t.Fatalf("LaunchPasswordCaptcha remote error = %v", err)
	}
	if gotDisplay != "" {
		t.Fatalf("remote CAPTCHA launched before authenticated viewer with display %q", gotDisplay)
	}
}

func TestDisabledCaptchaBrowserRejectsPasswordBeginWithoutStateOrLifecycleWorkerAndKeepsQRAvailable(t *testing.T) {
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

	captchaURL, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "sensitive-riot-user", "sensitive-riot-password")
	if !errors.Is(err, ErrPasswordLoginDisabled) {
		t.Fatalf("BeginPasswordLogin disabled error = %v", err)
	}
	if captchaURL != "" || state != "" {
		t.Fatalf("disabled password begin returned URL/state %q/%q", captchaURL, state)
	}
	if strings.Contains(err.Error(), "sensitive-riot-user") || strings.Contains(err.Error(), "sensitive-riot-password") {
		t.Fatalf("disabled password error retained credentials: %q", err)
	}
	s.mu.Lock()
	retained := len(s.passwordPending) + len(s.passwordOutcomes) + len(s.passwordReady)
	s.mu.Unlock()
	if retained != 0 {
		t.Fatalf("disabled password begin retained %d state entries", retained)
	}
	lifecycleDrained := make(chan struct{})
	go func() {
		s.lifecycleWG.Wait()
		close(lifecycleDrained)
	}()
	select {
	case <-lifecycleDrained:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("disabled password begin enrolled a lifecycle worker")
	}

	if _, _, err := s.BeginQRAuth(context.Background(), "owner-1"); err != nil {
		t.Fatalf("BeginQRAuth while CAPTCHA disabled: %v", err)
	}
}
