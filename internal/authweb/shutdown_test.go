package authweb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
)

type retryCloseController struct {
	calls atomic.Int32
}

func (c *retryCloseController) Close() error {
	if c.calls.Add(1) == 1 {
		return errors.New("transient browser cleanup failure")
	}
	return nil
}

func TestRetainedCaptchaBrowserIsReapedAfterTransientCloseFailure(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	controller := &retryCloseController{}
	flowCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	flow := &passwordFlow{ctx: flowCtx, cancel: cancel, browser: controller}
	if err := s.closeOwnedCaptchaBrowser(flow); err == nil {
		t.Fatal("first close unexpectedly succeeded")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		_, retained := s.captchaCloseFailures[flow]
		s.mu.Unlock()
		flow.launchMu.Lock()
		browser := flow.browser
		flow.launchMu.Unlock()
		if controller.calls.Load() >= 2 && !retained && browser == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reaper did not reclaim browser: calls=%d retained=%v browser=%T", controller.calls.Load(), retained, browser)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServerShutdownStopsOwnedTLSWakesPasswordWaiterAndIsIdempotent(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	if err := s.listenCaptchaTLS(0, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	port := s.captchaTLSPort
	s.mu.Unlock()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := tls.Dial("tcp", addr, &tls.Config{ //nolint:gosec -- self-signed test server
		InsecureSkipVerify: true,
		ServerName:         RiotCaptchaHost,
	})
	if err != nil {
		t.Fatalf("owned TLS did not accept a connection: %v", err)
	}
	_ = conn.Close()

	controller := newTestCaptchaBrowserController()
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() {
		_, _, _, err := s.WaitPasswordLogin(context.Background(), state)
		waitDone <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close after Shutdown: %v", err)
	}

	select {
	case err := <-waitDone:
		if !errors.Is(err, ErrServerClosed) {
			t.Fatalf("password waiter error=%v, want ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("password waiter was not woken by shutdown")
	}
	if got := controller.closeCalls.Load(); got != 1 {
		t.Fatalf("browser close calls=%d, want 1", got)
	}
	if conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("owned CAPTCHA TLS still accepts connections after shutdown")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.passwordPending) != 0 || len(s.passwordOutcomes) != 0 || len(s.passwordReady) != 0 ||
		len(s.mfaPending) != 0 || len(s.qrSessions) != 0 {
		t.Fatalf("shutdown retained auth state: password=%d outcomes=%d ready=%d mfa=%d qr=%d",
			len(s.passwordPending), len(s.passwordOutcomes), len(s.passwordReady), len(s.mfaPending), len(s.qrSessions))
	}
}

func TestServerShutdownCancelsMFAAndDoesNotHoldMutexDuringBrowserClose(t *testing.T) {
	mfaStarted := make(chan struct{}, 1)
	pw := &fakePasswordAuth{
		mfaStarted: mfaStarted,
		mfaRelease: make(chan struct{}),
	}
	s := newCaptchaServer(pw)
	controllerRelease := make(chan struct{})
	controller := newTestCaptchaBrowserController()
	controller.closeRelease = controllerRelease
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}

	mfaCtx, mfaCancel := context.WithCancel(context.Background())
	mfaFlow := &mfaFlow{ctx: mfaCtx, cancel: mfaCancel}
	s.mu.Lock()
	s.mfaPending["mfa-state"] = mfaPending{
		discordUserID: "owner-1",
		challenge:     &riot.MFAChallenge{Method: "email"},
		flow:          mfaFlow,
		expiresAt:     time.Now().Add(time.Hour),
	}
	s.mu.Unlock()
	mfaDone := make(chan error, 1)
	go func() {
		_, err := s.CompletePasswordMFA(context.Background(), "mfa-state", "owner-1", "123456")
		mfaDone <- err
	}()
	select {
	case <-mfaStarted:
	case <-time.After(time.Second):
		t.Fatal("MFA request did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case <-controller.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not start browser close")
	}
	lockDone := make(chan struct{})
	go func() {
		s.mu.Lock()
		s.mu.Unlock()
		close(lockDone)
	}()
	select {
	case <-lockDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Server.mu was held while browser Close blocked")
	}
	close(controllerRelease)
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not join canceled operations")
	}
	select {
	case err := <-mfaDone:
		if !errors.Is(err, ErrServerClosed) {
			t.Fatalf("MFA error=%v, want ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("MFA operation was not canceled")
	}
}

func TestServerShutdownRejectsNewPasswordAndQRWork(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass"); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("BeginPasswordLogin error=%v, want ErrServerClosed", err)
	}
	if _, _, err := s.BeginQRAuth(context.Background(), "owner-1"); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("BeginQRAuth error=%v, want ErrServerClosed", err)
	}
}
