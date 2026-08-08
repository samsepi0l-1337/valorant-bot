package authweb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
)

type manualRemoteCaptchaClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []chan time.Time
}

type lockedRemoteCaptchaReader struct {
	mu     sync.Mutex
	reader *bytes.Reader
}

func (r *lockedRemoteCaptchaReader) Read(destination []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reader.Read(destination)
}

func newManualRemoteCaptchaClock(now time.Time) *manualRemoteCaptchaClock {
	return &manualRemoteCaptchaClock{now: now}
}

func (c *manualRemoteCaptchaClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualRemoteCaptchaClock) After(time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	waiter := make(chan time.Time, 1)
	c.waiters = append(c.waiters, waiter)
	return waiter
}

func (c *manualRemoteCaptchaClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	waiters := c.waiters
	c.waiters = nil
	c.mu.Unlock()
	for _, waiter := range waiters {
		waiter <- now
	}
}

func newRemoteCaptchaStateServer(t *testing.T, entropy []byte, ttl time.Duration) *Server {
	t.Helper()
	s := New(Deps{
		AuthBaseURL:        "https://relay.example.com",
		CaptchaBrowserMode: netutil.CaptchaBrowserRemote,
		PasswordAuth:       &fakePasswordAuth{},
		PendingTTL:         ttl,
		Store:              newMockStore(),
		Riot:               &mockRiot{},
		Boxer:              &mockBoxer{},
	})
	s.setRemoteCaptchaHooksForTest(remoteCaptchaHooks{
		random: &lockedRemoteCaptchaReader{reader: bytes.NewReader(entropy)},
		now:    time.Now,
		after:  time.After,
	})
	t.Cleanup(func() {
		_ = s.Close()
		s.clearRemoteCaptchaHooksForTest()
	})
	return s
}

func remoteBearerFromURL(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Host != "relay.example.com" || parsed.Path != "/captcha/remote" {
		t.Fatalf("remote URL = %q", rawURL)
	}
	if parsed.RawQuery != "" || parsed.Fragment == "" {
		t.Fatalf("remote bearer must be fragment-only: %q", rawURL)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parsed.Fragment)
	if err != nil || len(decoded) != remoteCaptchaSecretBytes {
		t.Fatalf("remote bearer = %q decoded=%d err=%v", parsed.Fragment, len(decoded), err)
	}
	return parsed.Fragment
}

func TestBeginPasswordLoginRemoteIssuesFragmentBearerAndLocalReturnsNoURL(t *testing.T) {
	grantBytes := bytes.Repeat([]byte{0x31}, remoteCaptchaSecretBytes)
	remote := newRemoteCaptchaStateServer(t, grantBytes, time.Minute)

	remoteURL, state, err := remote.BeginPasswordLogin(context.Background(), "discord-owner", "riot-user", "riot-password")
	if err != nil {
		t.Fatal(err)
	}
	bearer := remoteBearerFromURL(t, remoteURL)
	wantBearer := base64.RawURLEncoding.EncodeToString(grantBytes)
	if bearer != wantBearer {
		t.Fatalf("bearer = %q, want deterministic 32-byte value", bearer)
	}

	remote.mu.Lock()
	pending := remote.passwordPending[state]
	remote.mu.Unlock()
	wantDigest := sha256.Sum256(grantBytes)
	if !pending.remoteGrant.active || pending.remoteGrant.digest != wantDigest {
		t.Fatalf("retained grant = %+v, want SHA-256 digest", pending.remoteGrant)
	}
	if pending.remoteGrant.ownerDiscordUserID != "discord-owner" || pending.remoteGrant.flow != pending.flow {
		t.Fatal("remote grant was not bound to the Discord owner and password flow")
	}
	if strings.Contains(pending.username, bearer) || strings.Contains(pending.password, bearer) {
		t.Fatal("raw remote bearer was retained outside the URL fragment")
	}

	local := newCaptchaServer(&fakePasswordAuth{})
	t.Cleanup(func() { _ = local.Close() })
	localURL, _, err := local.BeginPasswordLogin(context.Background(), "discord-owner", "riot-user", "riot-password")
	if err != nil {
		t.Fatal(err)
	}
	if localURL != "" {
		t.Fatalf("local mode exposed remote URL %q", localURL)
	}
}

func TestRemoteCaptchaGrantRedeemsOnceAndBindsOneOpaqueViewer(t *testing.T) {
	grantBytes := bytes.Repeat([]byte{0x41}, remoteCaptchaSecretBytes)
	viewerBytes := bytes.Repeat([]byte{0x52}, remoteCaptchaSecretBytes)
	rejectedViewerBytes := bytes.Repeat([]byte{0x53}, remoteCaptchaSecretBytes)
	entropy := append(append(append([]byte{}, grantBytes...), viewerBytes...), rejectedViewerBytes...)
	s := newRemoteCaptchaStateServer(t, entropy, time.Minute)

	remoteURL, state, err := s.BeginPasswordLogin(context.Background(), "discord-owner", "riot-user", "riot-password")
	if err != nil {
		t.Fatal(err)
	}
	bearer := remoteBearerFromURL(t, remoteURL)
	viewer, rawSession, err := s.redeemRemoteCaptchaGrant(bearer)
	if err != nil {
		t.Fatal(err)
	}
	if viewer.state != state || viewer.discordUserID != "discord-owner" || viewer.flow == nil {
		t.Fatalf("viewer binding = %+v", viewer)
	}
	wantSession := base64.RawURLEncoding.EncodeToString(viewerBytes)
	if rawSession != wantSession || rawSession == bearer {
		t.Fatalf("opaque viewer session = %q", rawSession)
	}

	s.mu.Lock()
	pending := s.passwordPending[state]
	s.mu.Unlock()
	if pending.remoteGrant.active || pending.remoteGrant.digest != ([sha256.Size]byte{}) {
		t.Fatalf("one-time grant remained active: %+v", pending.remoteGrant)
	}
	wantSessionDigest := sha256.Sum256(viewerBytes)
	if !pending.remoteViewer.active || pending.remoteViewer.digest != wantSessionDigest {
		t.Fatalf("retained viewer = %+v, want digest only", pending.remoteViewer)
	}
	if _, _, err := s.redeemRemoteCaptchaGrant(bearer); !errors.Is(err, errRemoteCaptchaUnavailable) {
		t.Fatalf("second redemption error = %v", err)
	}
	got, err := s.lookupRemoteCaptchaViewer(rawSession)
	if err != nil || got.state != state || got.flow != viewer.flow {
		t.Fatalf("viewer lookup = %+v, %v", got, err)
	}
}

func TestRemoteCaptchaGrantRejectsOwnerOrFlowReplacement(t *testing.T) {
	for _, replacement := range []string{"owner", "flow"} {
		t.Run(replacement, func(t *testing.T) {
			grantBytes := bytes.Repeat([]byte{0x61}, remoteCaptchaSecretBytes)
			viewerBytes := bytes.Repeat([]byte{0x62}, remoteCaptchaSecretBytes)
			s := newRemoteCaptchaStateServer(t, append(grantBytes, viewerBytes...), time.Minute)
			remoteURL, state, err := s.BeginPasswordLogin(context.Background(), "discord-owner", "riot-user", "riot-password")
			if err != nil {
				t.Fatal(err)
			}
			bearer := remoteBearerFromURL(t, remoteURL)

			s.mu.Lock()
			pending := s.passwordPending[state]
			if replacement == "owner" {
				pending.discordUserID = "different-owner"
			} else {
				replacementCtx, replacementCancel := context.WithCancel(context.Background())
				t.Cleanup(replacementCancel)
				pending.flow = &passwordFlow{ctx: replacementCtx, cancel: replacementCancel}
			}
			s.passwordPending[state] = pending
			s.mu.Unlock()
			if _, _, err := s.redeemRemoteCaptchaGrant(bearer); !errors.Is(err, errRemoteCaptchaUnavailable) {
				t.Fatalf("%s replacement redemption error = %v", replacement, err)
			}
		})
	}
}

func TestRemoteCaptchaCancellationAndTerminalOutcomeRemoveViewerState(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner cancellation", true: "terminal outcome"}[terminal], func(t *testing.T) {
			grantBytes := bytes.Repeat([]byte{0x71}, remoteCaptchaSecretBytes)
			viewerBytes := bytes.Repeat([]byte{0x72}, remoteCaptchaSecretBytes)
			s := newRemoteCaptchaStateServer(t, append(grantBytes, viewerBytes...), time.Minute)
			remoteURL, state, err := s.BeginPasswordLogin(context.Background(), "discord-owner", "riot-user", "riot-password")
			if err != nil {
				t.Fatal(err)
			}
			_, rawSession, err := s.redeemRemoteCaptchaGrant(remoteBearerFromURL(t, remoteURL))
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			flow := s.passwordPending[state].flow
			s.mu.Unlock()
			if terminal {
				if _, err := s.setPasswordOutcome(state, flow, passwordOutcome{err: errors.New("terminal")}); err != nil {
					t.Fatal(err)
				}
			} else if err := s.CancelPasswordLogin(state, "discord-owner"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.lookupRemoteCaptchaViewer(rawSession); !errors.Is(err, errRemoteCaptchaUnavailable) {
				t.Fatalf("viewer survived terminal transition: %v", err)
			}
			select {
			case <-flow.remoteDone:
			default:
				t.Fatal("remote lifecycle watcher was not released by terminal transition")
			}
			if terminal {
				s.mu.Lock()
				pending := s.passwordPending[state]
				s.mu.Unlock()
				if pending.remoteGrant.active || pending.remoteViewer.active ||
					pending.remoteGrant.digest != ([sha256.Size]byte{}) ||
					pending.remoteViewer.digest != ([sha256.Size]byte{}) {
					t.Fatal("terminal transition retained a remote credential digest")
				}
			}
		})
	}
}

func TestRemoteCaptchaExpiresAtConfiguredOrTenMinuteLifetimeWithDeterministicClock(t *testing.T) {
	for _, test := range []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{name: "configured lifetime", ttl: 2 * time.Minute, want: 2 * time.Minute},
		{name: "ten minute maximum", ttl: 30 * time.Minute, want: remoteCaptchaMaxLifetime},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualRemoteCaptchaClock(time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC))
			s := newRemoteCaptchaStateServer(t, bytes.Repeat([]byte{0x7a}, remoteCaptchaSecretBytes), test.ttl)
			s.setRemoteCaptchaHooksForTest(remoteCaptchaHooks{
				random: &lockedRemoteCaptchaReader{reader: bytes.NewReader(bytes.Repeat([]byte{0x7a}, remoteCaptchaSecretBytes))},
				now:    clock.Now,
				after:  clock.After,
			})

			_, state, err := s.BeginPasswordLogin(context.Background(), "discord-owner", "riot-user", "riot-password")
			if err != nil {
				t.Fatal(err)
			}
			clock.Advance(test.want)

			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				s.mu.Lock()
				_, exists := s.passwordPending[state]
				s.mu.Unlock()
				if !exists {
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("remote CAPTCHA state survived its fixed maximum lifetime")
		})
	}
}

func TestRemoteCaptchaShutdownRemovesGrantAndViewerState(t *testing.T) {
	grantBytes := bytes.Repeat([]byte{0x44}, remoteCaptchaSecretBytes)
	viewerBytes := bytes.Repeat([]byte{0x45}, remoteCaptchaSecretBytes)
	s := newRemoteCaptchaStateServer(t, append(grantBytes, viewerBytes...), time.Minute)
	remoteURL, state, err := s.BeginPasswordLogin(context.Background(), "discord-owner", "riot-user", "riot-password")
	if err != nil {
		t.Fatal(err)
	}
	_, rawSession, err := s.redeemRemoteCaptchaGrant(remoteBearerFromURL(t, remoteURL))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	_, pending := s.passwordPending[state]
	s.mu.Unlock()
	if pending {
		t.Fatal("password state survived server shutdown")
	}
	if _, err := s.lookupRemoteCaptchaViewer(rawSession); !errors.Is(err, errRemoteCaptchaUnavailable) {
		t.Fatalf("viewer lookup after shutdown = %v", err)
	}
}

func TestRemoteCaptchaConcurrentRedemptionBindsExactlyOneViewer(t *testing.T) {
	entropy := make([]byte, 0, remoteCaptchaSecretBytes*3)
	entropy = append(entropy, bytes.Repeat([]byte{0x81}, remoteCaptchaSecretBytes)...)
	entropy = append(entropy, bytes.Repeat([]byte{0x82}, remoteCaptchaSecretBytes)...)
	entropy = append(entropy, bytes.Repeat([]byte{0x83}, remoteCaptchaSecretBytes)...)
	s := newRemoteCaptchaStateServer(t, entropy, time.Minute)
	remoteURL, _, err := s.BeginPasswordLogin(context.Background(), "discord-owner", "riot-user", "riot-password")
	if err != nil {
		t.Fatal(err)
	}
	bearer := remoteBearerFromURL(t, remoteURL)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, _, redeemErr := s.redeemRemoteCaptchaGrant(bearer)
			results <- redeemErr
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if redeemErr := <-results; redeemErr == nil {
			successes++
		} else if !errors.Is(redeemErr, errRemoteCaptchaUnavailable) {
			t.Fatalf("unexpected redemption error: %v", redeemErr)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent redemptions = %d, want 1", successes)
	}
}
