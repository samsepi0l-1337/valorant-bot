package authweb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
)

type retryCloseController struct {
	calls atomic.Int32
}

type blockingRetryCloseController struct {
	calls   atomic.Int32
	started chan struct{}
	release <-chan struct{}
}

type alwaysFailCloseController struct {
	calls atomic.Int32
}

type shutdownBlockingRiot struct {
	*mockRiot
	namesStarted  chan struct{}
	namesRelease  <-chan struct{}
	namesReturned atomic.Bool
}

func (r *shutdownBlockingRiot) GetPlayerNames(ctx context.Context, accessToken, entitlementsToken, shard string, puuids []string) ([]riot.PlayerName, error) {
	select {
	case r.namesStarted <- struct{}{}:
	default:
	}
	defer r.namesReturned.Store(true)
	select {
	case <-r.namesRelease:
		return r.mockRiot.GetPlayerNames(ctx, accessToken, entitlementsToken, shard, puuids)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *retryCloseController) Close() error {
	if c.calls.Add(1) == 1 {
		return errors.New("transient browser cleanup failure")
	}
	return nil
}

func (c *blockingRetryCloseController) Close() error {
	if c.calls.Add(1) == 1 {
		close(c.started)
		<-c.release
		return errors.New("transient browser cleanup failure")
	}
	return nil
}

func (c *alwaysFailCloseController) Close() error {
	c.calls.Add(1)
	return errors.New("persistent browser cleanup failure")
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

func TestRetainedCaptchaBrowserFailureDuringIdleReaperExitStartsSuccessor(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	idleExit := make(chan struct{})
	releaseIdleExit := make(chan struct{})
	var releaseOnce sync.Once
	releaseExit := func() { releaseOnce.Do(func() { close(releaseIdleExit) }) }
	t.Cleanup(releaseExit)
	var hookCalls atomic.Int32
	s.beforeCaptchaReaperIdleExit = func() {
		if hookCalls.Add(1) != 1 {
			return
		}
		close(idleExit)
		<-releaseIdleExit
	}
	s.mu.Lock()
	s.captchaReaperRunning = true
	s.lifecycleWG.Add(1)
	s.mu.Unlock()
	initialReaperDone := make(chan struct{})
	go func() {
		s.reapRetainedCaptchaBrowsers()
		close(initialReaperDone)
	}()
	select {
	case <-idleExit:
	case <-time.After(time.Second):
		t.Fatal("empty retained-browser reaper did not reach idle exit")
	}

	flowCtx, cancelFlow := context.WithCancel(context.Background())
	controller := &retryCloseController{}
	flow := &passwordFlow{ctx: flowCtx, cancel: cancelFlow, browser: controller}
	t.Cleanup(func() {
		cancelFlow()
		_ = s.Close()
	})
	if err := s.closeOwnedCaptchaBrowser(flow); err == nil {
		t.Fatal("first browser close unexpectedly succeeded")
	}
	releaseExit()
	select {
	case <-initialReaperDone:
	case <-time.After(time.Second):
		t.Fatal("initial retained-browser reaper did not exit")
	}

	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		_, retained := s.captchaCloseFailures[flow]
		s.mu.Unlock()
		flow.launchMu.Lock()
		owned := flow.browser
		flow.launchMu.Unlock()
		if controller.calls.Load() >= 2 && !retained && owned == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle-exit close failure missed successor reaper: calls=%d retained=%v owned=%T", controller.calls.Load(), retained, owned)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRetainedCaptchaBrowserFailureAfterFinalSnapshotGetsOwnBoundedRetries(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	maxExit := make(chan struct{})
	releaseMaxExit := make(chan struct{})
	var releaseOnce sync.Once
	releaseExit := func() { releaseOnce.Do(func() { close(releaseMaxExit) }) }
	t.Cleanup(releaseExit)
	var hookCalls atomic.Int32
	s.beforeCaptchaReaperMaxExit = func() {
		if hookCalls.Add(1) != 1 {
			return
		}
		close(maxExit)
		<-releaseMaxExit
	}

	stuckCtx, cancelStuck := context.WithCancel(context.Background())
	stuckController := &alwaysFailCloseController{}
	stuckFlow := &passwordFlow{ctx: stuckCtx, cancel: cancelStuck, browser: stuckController}
	if err := s.closeOwnedCaptchaBrowser(stuckFlow); err == nil {
		t.Fatal("persistent browser close unexpectedly succeeded")
	}
	select {
	case <-maxExit:
	case <-time.After(2 * time.Second):
		t.Fatal("retained-browser reaper did not reach its final bounded snapshot")
	}

	newCtx, cancelNew := context.WithCancel(context.Background())
	newController := &retryCloseController{}
	newFlow := &passwordFlow{ctx: newCtx, cancel: cancelNew, browser: newController}
	t.Cleanup(func() {
		cancelStuck()
		cancelNew()
		_ = s.Close()
	})
	if err := s.closeOwnedCaptchaBrowser(newFlow); err == nil {
		t.Fatal("new browser's first close unexpectedly succeeded")
	}
	releaseExit()

	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		_, retained := s.captchaCloseFailures[newFlow]
		s.mu.Unlock()
		newFlow.launchMu.Lock()
		owned := newFlow.browser
		newFlow.launchMu.Unlock()
		if newController.calls.Load() >= 2 && !retained && owned == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new final-snapshot failure received no successor retries: calls=%d retained=%v owned=%T", newController.calls.Load(), retained, owned)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls := stuckController.calls.Load(); calls > 6 {
		t.Fatalf("persistent controller exceeded bounded background retries: calls=%d", calls)
	}
}

func TestServerShutdownJoinsCanceledPasswordWaiterCleanupBeforeFinalBrowserRetry(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseClose := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseClose)
	controller := &blockingRetryCloseController{
		started: make(chan struct{}),
		release: release,
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	flow.launchMu.Lock()
	flow.browser = controller
	flow.launchMu.Unlock()

	waitCtx, cancelWait := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() {
		_, _, _, err := s.WaitPasswordLogin(waitCtx, state)
		waitDone <- err
	}()
	cancelWait()
	select {
	case <-controller.started:
	case <-time.After(time.Second):
		t.Fatal("canceled password waiter did not reach browser cleanup")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		releaseClose()
		t.Fatalf("Shutdown returned before password waiter cleanup completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	releaseClose()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not join waiter cleanup and retry retained browser")
	}
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("password waiter error=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled password waiter did not return")
	}
	if calls := controller.calls.Load(); calls < 2 {
		t.Fatalf("browser close calls=%d, want final shutdown retry", calls)
	}
	flow.launchMu.Lock()
	owned := flow.browser
	flow.launchMu.Unlock()
	if owned != nil {
		t.Fatalf("shutdown retained browser %T after successful retry", owned)
	}
	s.mu.Lock()
	_, retained := s.captchaCloseFailures[flow]
	s.mu.Unlock()
	if retained {
		t.Fatal("shutdown retained a successful browser cleanup failure record")
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

func TestServerShutdownForceClosesStalledTLSConnection(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	if err := s.listenCaptchaTLS(0, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	port := s.captchaTLSPort
	s.mu.Unlock()
	conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{ //nolint:gosec -- self-signed test server
		InsecureSkipVerify: true,
		ServerName:         RiotCaptchaHost,
	})
	if err != nil {
		t.Fatalf("connect to owned TLS: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET /captcha/widget HTTP/1.1\r\nHost: %s\r\n", RiotCaptchaHost); err != nil {
		t.Fatalf("write partial request header: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	err = s.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v, want graceful TLS timeout", err)
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("Shutdown took %v; outer caller deadline fired instead of the CAPTCHA TLS deadline", elapsed)
	}

	if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	_, err = conn.Read(one[:])
	if err == nil {
		t.Fatal("stalled TLS connection remained readable after shutdown")
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("stalled TLS connection was not force-closed after graceful shutdown timed out")
	}
}

func TestCaptchaTLSServerBoundsHeaderAndIdleWaits(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	if err := s.listenCaptchaTLS(0, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	tlsServer := s.captchaTLSServer
	s.mu.Unlock()
	if tlsServer == nil {
		t.Fatal("owned CAPTCHA TLS server was not retained")
	}
	if tlsServer.ReadHeaderTimeout <= 0 {
		t.Fatal("CAPTCHA TLS ReadHeaderTimeout is unbounded")
	}
	if tlsServer.IdleTimeout <= 0 {
		t.Fatal("CAPTCHA TLS IdleTimeout is unbounded")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
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

func TestServerShutdownCancelsAndJoinsConsumedMFAAccountLink(t *testing.T) {
	pw := &fakePasswordAuth{tokens: riot.PasswordTokens{
		AccessToken:   "access-token",
		IDToken:       "id-token",
		SessionCookie: "ssid=session",
	}}
	s := newCaptchaServer(pw)
	namesRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseNames := func() { releaseOnce.Do(func() { close(namesRelease) }) }
	t.Cleanup(releaseNames)
	blockingRiot := &shutdownBlockingRiot{
		mockRiot: &mockRiot{
			entitlements: "entitlements",
			puuid:        "puuid-1",
			names:        []riot.PlayerName{{GameName: "Player", TagLine: "KR1"}},
			region:       "kr",
			shard:        "kr",
		},
		namesStarted: make(chan struct{}, 1),
		namesRelease: namesRelease,
	}
	s.riot = blockingRiot

	mfaCtx, mfaCancel := context.WithCancel(context.Background())
	mfaFlow := &mfaFlow{ctx: mfaCtx, cancel: mfaCancel}
	s.mu.Lock()
	s.mfaPending["mfa-consumed"] = mfaPending{
		discordUserID: "owner-1",
		challenge:     &riot.MFAChallenge{Method: "email"},
		flow:          mfaFlow,
		expiresAt:     time.Now().Add(time.Hour),
	}
	s.mu.Unlock()

	mfaDone := make(chan error, 1)
	go func() {
		_, err := s.CompletePasswordMFA(context.Background(), "mfa-consumed", "owner-1", "123456")
		mfaDone <- err
	}()
	select {
	case <-blockingRiot.namesStarted:
	case <-time.After(time.Second):
		t.Fatal("consumed MFA account link did not reach identity lookup")
	}
	s.mu.Lock()
	_, stillPending := s.mfaPending["mfa-consumed"]
	s.mu.Unlock()
	if stillPending {
		t.Fatal("test did not reach the post-consume MFA phase")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		releaseNames()
		t.Fatal("shutdown did not cancel the consumed MFA account link")
	}
	if !blockingRiot.namesReturned.Load() {
		releaseNames()
		select {
		case <-mfaDone:
		case <-time.After(time.Second):
		}
		t.Fatal("shutdown returned before the consumed MFA account link was canceled and joined")
	}
	select {
	case err := <-mfaDone:
		if !errors.Is(err, ErrServerClosed) {
			t.Fatalf("consumed MFA error=%v, want ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("joined MFA operation did not return")
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
