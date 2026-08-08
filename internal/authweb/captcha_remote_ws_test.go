package authweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/gorilla/websocket"
)

type testRemoteCaptchaStream struct {
	frames       chan []byte
	done         chan struct{}
	closeOnce    sync.Once
	inputs       chan []byte
	dispatches   chan []byte
	dispatchErr  error
	closeStarted chan struct{}
	closeRelease <-chan struct{}
}

type blockingRemoteCaptchaWebSocketWriter struct {
	firstWriteStarted chan struct{}
	releaseFirstWrite chan struct{}
	firstOnce         sync.Once
	mu                sync.Mutex
	activeWrites      int
	maxActiveWrites   int
	frames            [][]byte
	controls          []int
	deadlines         []time.Time
}

type observedRemoteCaptchaWebSocketConnection struct {
	mu             sync.Mutex
	readLimit      int64
	readDeadlines  []time.Time
	writeDeadlines []time.Time
	pongHandler    func(string) error
	controls       []int
	closed         chan struct{}
	closeOnce      sync.Once
}

type testRemoteCaptchaDisconnectTimer struct {
	ch       <-chan time.Time
	stopOnce sync.Once
	stopped  chan<- struct{}
}

func (t *testRemoteCaptchaDisconnectTimer) Chan() <-chan time.Time { return t.ch }

func (t *testRemoteCaptchaDisconnectTimer) Stop() bool {
	stopped := false
	t.stopOnce.Do(func() {
		stopped = true
		t.stopped <- struct{}{}
	})
	return stopped
}

func newObservedRemoteCaptchaWebSocketConnection() *observedRemoteCaptchaWebSocketConnection {
	return &observedRemoteCaptchaWebSocketConnection{closed: make(chan struct{})}
}

func (c *observedRemoteCaptchaWebSocketConnection) SetReadLimit(limit int64) {
	c.mu.Lock()
	c.readLimit = limit
	c.mu.Unlock()
}

func (c *observedRemoteCaptchaWebSocketConnection) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadlines = append(c.readDeadlines, deadline)
	c.mu.Unlock()
	return nil
}

func (c *observedRemoteCaptchaWebSocketConnection) SetPongHandler(handler func(string) error) {
	c.mu.Lock()
	c.pongHandler = handler
	c.mu.Unlock()
}

func (c *observedRemoteCaptchaWebSocketConnection) ReadMessage() (int, []byte, error) {
	<-c.closed
	return 0, nil, errors.New("connection closed")
}

func (c *observedRemoteCaptchaWebSocketConnection) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadlines = append(c.writeDeadlines, deadline)
	c.mu.Unlock()
	return nil
}

func (c *observedRemoteCaptchaWebSocketConnection) WriteMessage(int, []byte) error { return nil }

func (c *observedRemoteCaptchaWebSocketConnection) WriteControl(messageType int, _ []byte, _ time.Time) error {
	c.mu.Lock()
	c.controls = append(c.controls, messageType)
	c.mu.Unlock()
	return nil
}

func (c *observedRemoteCaptchaWebSocketConnection) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func newBlockingRemoteCaptchaWebSocketWriter() *blockingRemoteCaptchaWebSocketWriter {
	return &blockingRemoteCaptchaWebSocketWriter{
		firstWriteStarted: make(chan struct{}),
		releaseFirstWrite: make(chan struct{}),
	}
}

func (w *blockingRemoteCaptchaWebSocketWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadlines = append(w.deadlines, deadline)
	w.mu.Unlock()
	return nil
}

func (w *blockingRemoteCaptchaWebSocketWriter) WriteMessage(messageType int, payload []byte) error {
	w.mu.Lock()
	w.activeWrites++
	if w.activeWrites > w.maxActiveWrites {
		w.maxActiveWrites = w.activeWrites
	}
	w.mu.Unlock()
	w.firstOnce.Do(func() {
		close(w.firstWriteStarted)
		<-w.releaseFirstWrite
	})
	w.mu.Lock()
	if messageType == websocket.BinaryMessage {
		w.frames = append(w.frames, append([]byte(nil), payload...))
	}
	w.activeWrites--
	w.mu.Unlock()
	return nil
}

func (w *blockingRemoteCaptchaWebSocketWriter) WriteControl(messageType int, _ []byte, _ time.Time) error {
	w.mu.Lock()
	w.activeWrites++
	if w.activeWrites > w.maxActiveWrites {
		w.maxActiveWrites = w.activeWrites
	}
	w.controls = append(w.controls, messageType)
	w.activeWrites--
	w.mu.Unlock()
	return nil
}

func newTestRemoteCaptchaStream() *testRemoteCaptchaStream {
	return &testRemoteCaptchaStream{
		frames:     make(chan []byte, 1),
		done:       make(chan struct{}),
		inputs:     make(chan []byte, 8),
		dispatches: make(chan []byte, 8),
	}
}

func (s *testRemoteCaptchaStream) Frames() <-chan []byte { return s.frames }

func (s *testRemoteCaptchaStream) DispatchInput(_ context.Context, payload []byte) error {
	s.dispatches <- append([]byte(nil), payload...)
	if s.dispatchErr != nil {
		return s.dispatchErr
	}
	s.inputs <- append([]byte(nil), payload...)
	return nil
}

func (s *testRemoteCaptchaStream) Done() <-chan struct{} { return s.done }

func (s *testRemoteCaptchaStream) Err() error { return nil }

func (s *testRemoteCaptchaStream) Close(context.Context) error {
	s.closeOnce.Do(func() {
		if s.closeStarted != nil {
			close(s.closeStarted)
		}
		if s.closeRelease != nil {
			<-s.closeRelease
		}
		close(s.done)
	})
	return nil
}

type testRemoteCaptchaBrowser struct {
	*testRiotBrowserLoginController
	processDone chan struct{}
	exitOnce    sync.Once
}

type defaultRemoteCaptchaControllerProbe struct {
	owners [4]context.Context
	err    error
}

func (*defaultRemoteCaptchaControllerProbe) Close() error { return nil }

func (p *defaultRemoteCaptchaControllerProbe) StartRemoteCaptchaStream(flowCtx, processCtx, viewerCtx, serverCtx context.Context) (*remoteCaptchaStream, error) {
	p.owners = [4]context.Context{flowCtx, processCtx, viewerCtx, serverCtx}
	return nil, p.err
}

func newTestRemoteCaptchaBrowser() *testRemoteCaptchaBrowser {
	base := newTestCaptchaBrowserController()
	browser := &testRemoteCaptchaBrowser{
		testRiotBrowserLoginController: &testRiotBrowserLoginController{testCaptchaBrowserController: base},
		processDone:                    make(chan struct{}),
	}
	base.onClose = func() { browser.exitOnce.Do(func() { close(browser.processDone) }) }
	browser.run = func(ctx context.Context, _, _ string) (riotBrowserLoginResult, error) {
		<-ctx.Done()
		return riotBrowserLoginResult{}, ctx.Err()
	}
	return browser
}

type remoteCaptchaWebSocketFixture struct {
	server         *Server
	httpServer     *httptest.Server
	origin         string
	cookie         *http.Cookie
	state          string
	browser        *testRemoteCaptchaBrowser
	stream         *testRemoteCaptchaStream
	store          *mockStore
	riot           *mockRiot
	boxer          *mockBoxer
	clock          *manualRemoteCaptchaClock
	launchCalls    atomic.Int32
	launchDisplays chan string
	streamCalls    atomic.Int32
	streamOwners   chan [4]context.Context
	graceTimers    chan chan time.Time
	graceDurations chan time.Duration
	graceChecks    chan struct{}
	graceStops     chan struct{}
}

func newRemoteCaptchaWebSocketFixture(t *testing.T) *remoteCaptchaWebSocketFixture {
	t.Helper()
	fixture := &remoteCaptchaWebSocketFixture{
		browser: newTestRemoteCaptchaBrowser(),
		stream:  newTestRemoteCaptchaStream(),
		store:   newMockStore(),
		riot: &mockRiot{
			entitlements: "entitlements",
			puuid:        "remote-puuid",
			names:        []riot.PlayerName{{GameName: "Remote", TagLine: "AP1"}},
			region:       "ap",
			shard:        "ap",
		},
		boxer:          &mockBoxer{},
		clock:          newManualRemoteCaptchaClock(time.Now()),
		streamOwners:   make(chan [4]context.Context, 1),
		launchDisplays: make(chan string, 1),
		graceTimers:    make(chan chan time.Time, 4),
		graceDurations: make(chan time.Duration, 4),
		graceChecks:    make(chan struct{}, 4),
		graceStops:     make(chan struct{}, 4),
	}
	fixture.httpServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.server.Handler().ServeHTTP(w, r)
	}))
	fixture.origin = "https://" + strings.TrimPrefix(fixture.httpServer.URL, "http://")
	passwordAuth := &fakeBrowserPasswordAuth{
		fakePasswordAuth: &fakePasswordAuth{},
		authorizeURL:     "https://auth.riotgames.com/authorize",
	}
	fixture.server = New(Deps{
		AuthBaseURL:        fixture.origin,
		CaptchaBrowserMode: netutil.CaptchaBrowserRemote,
		CaptchaDisplay:     ":99",
		PasswordAuth:       passwordAuth,
		PendingTTL:         time.Minute,
		Store:              fixture.store,
		Riot:               fixture.riot,
		Boxer:              fixture.boxer,
	})
	fixture.server.setRemoteCaptchaHooksForTest(remoteCaptchaHooks{
		random: &lockedRemoteCaptchaReader{reader: bytes.NewReader(bytes.Repeat([]byte{0x62}, remoteCaptchaSecretBytes*4))},
		now:    fixture.clock.Now,
		after:  fixture.clock.After,
	})
	fixture.server.launchRemoteCaptchaBrowser = func(_ string, display string) (captchaBrowserController, error) {
		fixture.launchCalls.Add(1)
		fixture.launchDisplays <- display
		return fixture.browser, nil
	}
	fixture.server.remoteCaptchaProcessDone = func(controller captchaBrowserController) <-chan struct{} {
		if controller != fixture.browser {
			t.Fatalf("process owner controller=%T, want fixture browser", controller)
		}
		return fixture.browser.processDone
	}
	fixture.server.remoteCaptchaStartStream = func(controller captchaBrowserController, flowCtx, processCtx, viewerCtx, serverCtx context.Context) (remoteCaptchaStreamSession, error) {
		if controller != fixture.browser {
			t.Fatalf("stream controller=%T, want fixture browser", controller)
		}
		fixture.streamCalls.Add(1)
		fixture.streamOwners <- [4]context.Context{flowCtx, processCtx, viewerCtx, serverCtx}
		return fixture.stream, nil
	}
	fixture.server.remoteCaptchaGraceTimer = func(duration time.Duration) remoteCaptchaDisconnectTimer {
		timer := make(chan time.Time, 1)
		fixture.graceDurations <- duration
		fixture.graceTimers <- timer
		return &testRemoteCaptchaDisconnectTimer{ch: timer, stopped: fixture.graceStops}
	}
	fixture.server.afterRemoteCaptchaGraceTimerForTest = func() {
		fixture.graceChecks <- struct{}{}
	}

	remoteURL, state, err := fixture.server.BeginPasswordLogin(context.Background(), "discord-owner", "riot-user", "riot-password")
	if err != nil {
		t.Fatal(err)
	}
	fixture.state = state
	bearer := strings.TrimPrefix(remoteURL, fixture.origin+"/captcha/remote#")
	if bearer == remoteURL || bearer == "" {
		t.Fatalf("remote URL=%q", remoteURL)
	}
	redeemBody := fmt.Sprintf(`{"token":%q}`, bearer)
	redeem := httptest.NewRequest(http.MethodPost, fixture.origin+"/api/auth/captcha/remote/redeem", strings.NewReader(redeemBody))
	redeem.Host = strings.TrimPrefix(fixture.origin, "https://")
	redeem.Header.Set("Origin", fixture.origin)
	redeem.Header.Set("Content-Type", "application/json")
	recorder := serveRemoteCaptchaHTTP(fixture.server, redeem)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("redeem status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	fixture.cookie = recorder.Result().Cookies()[0]
	t.Cleanup(func() {
		_ = fixture.server.Close()
		fixture.httpServer.Close()
		fixture.server.clearRemoteCaptchaHooksForTest()
	})
	return fixture
}

func (f *remoteCaptchaWebSocketFixture) nextGraceTimer(t *testing.T) chan time.Time {
	t.Helper()
	select {
	case duration := <-f.graceDurations:
		if duration != time.Minute {
			t.Fatalf("disconnect grace=%s, want 1m", duration)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not start grace timer")
	}
	select {
	case timer := <-f.graceTimers:
		return timer
	case <-time.After(time.Second):
		t.Fatal("disconnect grace timer missing")
		return nil
	}
}

func (f *remoteCaptchaWebSocketFixture) fireGraceTimer(t *testing.T, timer chan time.Time) {
	t.Helper()
	timer <- time.Now().Add(time.Minute)
	select {
	case <-f.graceChecks:
	case <-time.After(time.Second):
		t.Fatal("grace timer was not processed")
	}
}

func (f *remoteCaptchaWebSocketFixture) dial(t *testing.T, cookie *http.Cookie, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return f.dialContext(ctx, cookie, origin)
}

func (f *remoteCaptchaWebSocketFixture) dialContext(ctx context.Context, cookie *http.Cookie, origin string) (*websocket.Conn, *http.Response, error) {
	header := make(http.Header)
	header.Set("Origin", origin)
	if cookie != nil {
		header.Set("Cookie", cookie.String())
	}
	wsURL := "ws://" + strings.TrimPrefix(f.httpServer.URL, "http://") + "/api/auth/captcha/remote/ws"
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = time.Second
	return dialer.DialContext(ctx, wsURL, header)
}

func redeemedRemoteCaptchaViewerCookie(t *testing.T, s *Server, bearer string) *http.Cookie {
	t.Helper()
	body := fmt.Sprintf(`{"token":%q}`, bearer)
	recorder := serveRemoteCaptchaHTTP(s, remoteCaptchaHTTPRequest(http.MethodPost, "/api/auth/captcha/remote/redeem", remoteCaptchaTestOrigin, body))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("redeem status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("redeem cookies=%d, want 1", len(cookies))
	}
	return cookies[0]
}

func TestRemoteCaptchaWebSocketRejectsUntrustedRequestsBeforeUpgrade(t *testing.T) {
	s, bearer, _ := newRemoteCaptchaHTTPFixture(t)
	cookie := redeemedRemoteCaptchaViewerCookie(t, s, bearer)

	tests := []struct {
		name       string
		host       string
		origin     string
		cookie     *http.Cookie
		wantStatus int
	}{
		{name: "wrong host", host: "attacker.example", origin: remoteCaptchaTestOrigin, cookie: cookie, wantStatus: http.StatusForbidden},
		{name: "wrong origin", host: "relay.example.com", origin: "https://attacker.example", cookie: cookie, wantStatus: http.StatusForbidden},
		{name: "missing cookie", host: "relay.example.com", origin: remoteCaptchaTestOrigin, wantStatus: http.StatusUnauthorized},
		{name: "malformed cookie", host: "relay.example.com", origin: remoteCaptchaTestOrigin, cookie: &http.Cookie{Name: remoteCaptchaViewerCookieName, Value: "not-a-session"}, wantStatus: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := remoteCaptchaHTTPRequest(http.MethodGet, "/api/auth/captcha/remote/ws", test.origin, "")
			req.Host = test.host
			req.Header.Set("X-Forwarded-Host", "relay.example.com")
			req.Header.Set("X-Forwarded-Proto", "https")
			if test.cookie != nil {
				req.AddCookie(test.cookie)
			}
			recorder := serveRemoteCaptchaHTTP(s, req)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status=%d, want %d; body=%q", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if body := recorder.Body.String(); strings.Contains(body, cookie.Value) || strings.Contains(body, bearer) {
				t.Fatal("WebSocket authentication error disclosed a secret")
			}
		})
	}
}

func TestRemoteCaptchaWebSocketScrubsReplacedViewerBindingBeforeUpgrade(t *testing.T) {
	for _, replacement := range []string{"owner", "flow"} {
		t.Run(replacement, func(t *testing.T) {
			s, bearer, state := newRemoteCaptchaHTTPFixture(t)
			cookie := redeemedRemoteCaptchaViewerCookie(t, s, bearer)

			s.mu.Lock()
			pending := s.passwordPending[state]
			originalFlow := pending.flow
			var replacementFlow *passwordFlow
			if replacement == "owner" {
				pending.discordUserID = "different-owner"
			} else {
				replacementCtx, replacementCancel := context.WithCancel(context.Background())
				t.Cleanup(replacementCancel)
				replacementFlow = &passwordFlow{
					ctx:        replacementCtx,
					cancel:     replacementCancel,
					remoteDone: make(chan struct{}),
				}
				pending.flow = replacementFlow
			}
			s.passwordPending[state] = pending
			s.mu.Unlock()

			req := remoteCaptchaHTTPRequest(http.MethodGet, "/api/auth/captcha/remote/ws", remoteCaptchaTestOrigin, "")
			req.AddCookie(cookie)
			recorder := serveRemoteCaptchaHTTP(s, req)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%q, want 401", recorder.Code, recorder.Body.String())
			}

			s.mu.Lock()
			_, retained := s.passwordPending[state]
			s.mu.Unlock()
			if retained {
				t.Fatal("WebSocket binding mismatch retained password/viewer state")
			}
			assertRemoteCaptchaFlowReleased(t, originalFlow)
			if replacementFlow != nil {
				assertRemoteCaptchaFlowReleased(t, replacementFlow)
			}
			assertRemoteCaptchaLifecycleDrained(t, s)
		})
	}
}

func TestRemoteCaptchaWebSocketFirstAuthenticatedBindLaunchesOnceAndRejectsConcurrentViewer(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	if got := fixture.launchCalls.Load(); got != 0 {
		t.Fatalf("browser launched before WebSocket bind: %d", got)
	}
	if got := fixture.streamCalls.Load(); got != 0 {
		t.Fatalf("stream started before WebSocket bind: %d", got)
	}

	connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("first authenticated dial status=%d err=%v", status, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	select {
	case owners := <-fixture.streamOwners:
		for i, owner := range owners {
			if owner == nil || owner.Err() != nil {
				t.Fatalf("stream owner context %d invalid: %v", i, owner)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("remote stream did not start after authenticated bind")
	}
	if got := fixture.launchCalls.Load(); got != 1 {
		t.Fatalf("browser launches=%d, want 1", got)
	}
	select {
	case display := <-fixture.launchDisplays:
		if display != ":99" {
			t.Fatalf("remote CAPTCHA display=%q, want :99", display)
		}
	default:
		t.Fatal("remote CAPTCHA launch did not report its configured display")
	}
	if got := fixture.streamCalls.Load(); got != 1 {
		t.Fatalf("stream starts=%d, want 1", got)
	}

	second, secondResponse, secondErr := fixture.dial(t, fixture.cookie, fixture.origin)
	if second != nil {
		_ = second.Close()
	}
	if secondErr == nil || secondResponse == nil || secondResponse.StatusCode != http.StatusConflict {
		status := 0
		if secondResponse != nil {
			status = secondResponse.StatusCode
		}
		t.Fatalf("second concurrent dial status=%d err=%v, want 409", status, secondErr)
	}
	if got := fixture.launchCalls.Load(); got != 1 {
		t.Fatalf("concurrent viewer relaunched browser: %d", got)
	}
}

func TestDefaultRemoteCaptchaStartStreamForwardsAllFourOwnerContexts(t *testing.T) {
	wantErr := errors.New("probe stream start")
	controller := &defaultRemoteCaptchaControllerProbe{err: wantErr}
	owners := [4]context.Context{}
	for index := range owners {
		owners[index] = context.WithValue(context.Background(), struct{ index int }{index}, index)
	}
	_, err := defaultRemoteCaptchaStartStream(controller, owners[0], owners[1], owners[2], owners[3])
	if !errors.Is(err, wantErr) {
		t.Fatalf("default stream error=%v, want probe error", err)
	}
	for index := range owners {
		if controller.owners[index] != owners[index] {
			t.Fatalf("owner context %d was not forwarded by the default controller path", index)
		}
	}
}

func TestRemoteCaptchaWebSocketRelaysBinaryFramesAndTypedInput(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("authenticated dial status=%d err=%v", status, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	select {
	case <-fixture.streamOwners:
	case <-time.After(time.Second):
		t.Fatal("remote stream did not start")
	}

	wantFrame := []byte{0xff, 0xd8, 0xff, 0xdb, 0x00, 0x43, 0xff, 0xd9}
	fixture.stream.frames <- wantFrame
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	messageType, frame, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.BinaryMessage || !bytes.Equal(frame, wantFrame) {
		t.Fatalf("frame type=%d bytes=%x, want binary %x", messageType, frame, wantFrame)
	}

	inputs := [][]byte{
		[]byte(`{"type":"pointer","phase":"down","x":320,"y":225,"width":640,"height":450,"button":0}`),
		[]byte(`{"type":"wheel","x":320,"y":225,"width":640,"height":450,"deltaY":120}`),
	}
	for _, input := range inputs {
		if err := connection.WriteMessage(websocket.TextMessage, input); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-fixture.stream.inputs:
			if !bytes.Equal(got, input) {
				t.Fatalf("dispatched input=%q, want %q", got, input)
			}
		case <-time.After(time.Second):
			t.Fatal("typed input was not dispatched")
		}
	}
}

func TestRemoteCaptchaWebSocketReconnectsSameSessionInsideGraceAndExpiresAfterGrace(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	wantExpiresAt := fixture.clock.Now().Add(60 * time.Second)
	fixture.server.mu.Lock()
	pendingAtStart := fixture.server.passwordPending[fixture.state]
	if !pendingAtStart.remoteViewer.expiresAt.Equal(wantExpiresAt) {
		fixture.server.mu.Unlock()
		t.Fatalf("initial viewer expiresAt=%s, want independent literal %s", pendingAtStart.remoteViewer.expiresAt, wantExpiresAt)
	}
	fixture.server.mu.Unlock()
	first, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		t.Fatalf("first dial response=%v err=%v", response, err)
	}
	select {
	case <-fixture.streamOwners:
	case <-time.After(time.Second):
		t.Fatal("remote stream did not start")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	staleTimer := fixture.nextGraceTimer(t)
	fixture.server.mu.Lock()
	pending := fixture.server.passwordPending[fixture.state]
	staleGraceDone := pending.remoteViewer.relay.graceDone
	staleGraceCancel := pending.remoteViewer.relay.graceCancel
	fixture.server.mu.Unlock()
	if staleGraceDone == nil || staleGraceCancel == nil {
		t.Fatal("disconnect did not publish one cancelable grace worker")
	}

	otherCookie := &http.Cookie{Name: remoteCaptchaViewerCookieName, Value: strings.Repeat("A", 43)}
	other, otherResponse, otherErr := fixture.dial(t, otherCookie, fixture.origin)
	if other != nil {
		_ = other.Close()
	}
	if otherErr == nil || otherResponse == nil || otherResponse.StatusCode != http.StatusUnauthorized {
		status := 0
		if otherResponse != nil {
			status = otherResponse.StatusCode
		}
		t.Fatalf("different session dial status=%d err=%v, want 401", status, otherErr)
	}

	second, secondResponse, secondErr := fixture.dial(t, fixture.cookie, fixture.origin)
	if secondErr != nil {
		status := 0
		if secondResponse != nil {
			status = secondResponse.StatusCode
		}
		t.Fatalf("same-session reconnect status=%d err=%v", status, secondErr)
	}
	select {
	case <-staleGraceDone:
	default:
		t.Fatal("same-session reconnect returned before the stale grace worker terminated")
	}
	select {
	case <-fixture.graceStops:
	default:
		t.Fatal("same-session reconnect did not stop the stale grace timer")
	}
	fixture.server.mu.Lock()
	pending = fixture.server.passwordPending[fixture.state]
	if !pending.remoteViewer.expiresAt.Equal(wantExpiresAt) || !pending.remoteViewer.relay.expiresAt.Equal(wantExpiresAt) {
		fixture.server.mu.Unlock()
		t.Fatalf("reconnect changed absolute expiry: viewer=%s relay=%s want=%s", pending.remoteViewer.expiresAt, pending.remoteViewer.relay.expiresAt, wantExpiresAt)
	}
	if pending.remoteViewer.relay.graceDone != nil || pending.remoteViewer.relay.graceCancel != nil {
		fixture.server.mu.Unlock()
		t.Fatal("same-session reconnect retained a stale grace worker")
	}
	fixture.server.mu.Unlock()
	staleTimer <- time.Now().Add(time.Minute)
	select {
	case <-fixture.graceChecks:
		t.Fatal("canceled stale grace worker processed its timer after reconnect")
	case <-time.After(20 * time.Millisecond):
	}
	fixture.server.mu.Lock()
	_, stillLive := fixture.server.passwordPending[fixture.state]
	fixture.server.mu.Unlock()
	if !stillLive {
		t.Fatal("stale disconnect timer canceled a reconnected viewer")
	}
	if got := fixture.launchCalls.Load(); got != 1 {
		t.Fatalf("reconnect browser launches=%d, want 1", got)
	}
	if got := fixture.streamCalls.Load(); got != 1 {
		t.Fatalf("reconnect stream starts=%d, want 1", got)
	}

	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	finalTimer := fixture.nextGraceTimer(t)
	fixture.fireGraceTimer(t, finalTimer)
	select {
	case <-fixture.browser.closed:
	case <-time.After(time.Second):
		t.Fatal("grace expiry did not close the browser")
	}
	select {
	case <-fixture.stream.done:
	case <-time.After(time.Second):
		t.Fatal("grace expiry did not close the stream")
	}
	fixture.server.mu.Lock()
	_, stillLive = fixture.server.passwordPending[fixture.state]
	fixture.server.mu.Unlock()
	if stillLive {
		t.Fatal("grace expiry retained password state")
	}

	afterExpiry, expiryResponse, expiryErr := fixture.dial(t, fixture.cookie, fixture.origin)
	if afterExpiry != nil {
		_ = afterExpiry.Close()
	}
	if expiryErr == nil || expiryResponse == nil || expiryResponse.StatusCode != http.StatusUnauthorized {
		status := 0
		if expiryResponse != nil {
			status = expiryResponse.StatusCode
		}
		t.Fatalf("post-grace reconnect status=%d err=%v, want 401", status, expiryErr)
	}
}

// Mutation contract: removing stale-grace cancellation/joining, publishing a
// second timer, or failing to enroll the live grace worker in shutdown makes
// this bounded churn test fail.
func TestRemoteCaptchaWebSocketReconnectChurnOwnsOneGraceWorkerAndShutdownDrainsIt(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		t.Fatalf("initial dial response=%v err=%v", response, err)
	}
	select {
	case <-fixture.streamOwners:
	case <-time.After(time.Second):
		t.Fatal("remote stream did not start")
	}

	for cycle := 0; cycle < 3; cycle++ {
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
		_ = fixture.nextGraceTimer(t)
		fixture.server.mu.Lock()
		pending := fixture.server.passwordPending[fixture.state]
		staleDone := pending.remoteViewer.relay.graceDone
		if staleDone == nil || pending.remoteViewer.relay.graceCancel == nil {
			fixture.server.mu.Unlock()
			t.Fatalf("cycle %d did not publish exactly one cancelable grace worker", cycle)
		}
		fixture.server.mu.Unlock()
		select {
		case extra := <-fixture.graceDurations:
			t.Fatalf("cycle %d published overlapping grace timer %s", cycle, extra)
		default:
		}

		connection, response, err = fixture.dial(t, fixture.cookie, fixture.origin)
		if err != nil {
			t.Fatalf("cycle %d reconnect response=%v err=%v", cycle, response, err)
		}
		select {
		case <-staleDone:
		default:
			t.Fatalf("cycle %d reconnect returned before stale grace worker terminated", cycle)
		}
		select {
		case <-fixture.graceStops:
		case <-time.After(time.Second):
			t.Fatalf("cycle %d reconnect did not stop stale grace timer", cycle)
		}
		fixture.server.mu.Lock()
		pending = fixture.server.passwordPending[fixture.state]
		if pending.remoteViewer.relay.graceDone != nil || pending.remoteViewer.relay.graceCancel != nil {
			fixture.server.mu.Unlock()
			t.Fatalf("cycle %d reconnect retained stale grace ownership", cycle)
		}
		fixture.server.mu.Unlock()
	}

	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	_ = fixture.nextGraceTimer(t)
	fixture.server.mu.Lock()
	finalDone := fixture.server.passwordPending[fixture.state].remoteViewer.relay.graceDone
	fixture.server.mu.Unlock()
	if finalDone == nil {
		t.Fatal("final disconnect did not publish current grace worker")
	}
	closed := make(chan error, 1)
	go func() { closed <- fixture.server.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("shutdown during disconnect: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain the current grace worker")
	}
	select {
	case <-finalDone:
	default:
		t.Fatal("shutdown returned before current grace worker terminated")
	}
	select {
	case <-fixture.graceStops:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not stop current grace timer")
	}
	select {
	case extra := <-fixture.graceDurations:
		t.Fatalf("shutdown published an unexpected grace timer %s", extra)
	default:
	}
}

func TestRemoteCaptchaWebSocketReconnectAndShutdownJoinCanceledGraceBeforeReturn(t *testing.T) {
	t.Run("reconnect joins", func(t *testing.T) {
		fixture := newRemoteCaptchaWebSocketFixture(t)
		connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
		if err != nil {
			t.Fatalf("initial dial response=%v err=%v", response, err)
		}
		select {
		case <-fixture.streamOwners:
		case <-time.After(time.Second):
			t.Fatal("remote stream did not start")
		}
		hookEntered := make(chan struct{})
		hookRelease := make(chan struct{})
		var hookEnteredOnce sync.Once
		fixture.server.beforeRemoteCaptchaGraceDoneForTest = func() {
			hookEnteredOnce.Do(func() { close(hookEntered) })
			<-hookRelease
		}
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
		_ = fixture.nextGraceTimer(t)

		type dialResult struct {
			connection *websocket.Conn
			response   *http.Response
			err        error
		}
		dialDone := make(chan dialResult, 1)
		dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
		defer cancelDial()
		go func() {
			gotConnection, gotResponse, gotErr := fixture.dialContext(dialCtx, fixture.cookie, fixture.origin)
			dialDone <- dialResult{connection: gotConnection, response: gotResponse, err: gotErr}
		}()
		select {
		case <-hookEntered:
		case <-time.After(time.Second):
			close(hookRelease)
			t.Fatal("canceled grace worker did not reach the pre-done hook")
		}
		select {
		case early := <-dialDone:
			close(hookRelease)
			if early.connection != nil {
				_ = early.connection.Close()
			}
			t.Fatalf("reconnect returned before canceled grace worker joined: response=%v err=%v", early.response, early.err)
		case <-time.After(50 * time.Millisecond):
		}
		close(hookRelease)
		var reconnected dialResult
		select {
		case reconnected = <-dialDone:
		case <-time.After(time.Second):
			t.Fatal("reconnect did not return after canceled grace worker joined")
		}
		if reconnected.err != nil {
			t.Fatalf("reconnect response=%v err=%v", reconnected.response, reconnected.err)
		}
		fixture.server.beforeRemoteCaptchaGraceDoneForTest = nil
		if err := reconnected.connection.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("shutdown joins", func(t *testing.T) {
		fixture := newRemoteCaptchaWebSocketFixture(t)
		connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
		if err != nil {
			t.Fatalf("initial dial response=%v err=%v", response, err)
		}
		select {
		case <-fixture.streamOwners:
		case <-time.After(time.Second):
			t.Fatal("remote stream did not start")
		}
		hookEntered := make(chan struct{})
		hookRelease := make(chan struct{})
		var hookEnteredOnce sync.Once
		fixture.server.beforeRemoteCaptchaGraceDoneForTest = func() {
			hookEnteredOnce.Do(func() { close(hookEntered) })
			<-hookRelease
		}
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
		_ = fixture.nextGraceTimer(t)
		closeDone := make(chan error, 1)
		go func() { closeDone <- fixture.server.Close() }()
		select {
		case <-hookEntered:
		case <-time.After(time.Second):
			close(hookRelease)
			t.Fatal("shutdown-canceled grace worker did not reach the pre-done hook")
		}
		select {
		case err := <-closeDone:
			close(hookRelease)
			t.Fatalf("shutdown returned before grace worker joined: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(hookRelease)
		select {
		case err := <-closeDone:
			if err != nil {
				t.Fatalf("shutdown after grace join: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown did not return after grace worker joined")
		}
	})
}

func TestRemoteCaptchaWebSocketProtocolViolationsFailClosedWithoutGrace(t *testing.T) {
	tests := []struct {
		name        string
		messageType int
		payload     []byte
		dispatchErr error
	}{
		{name: "oversized text", messageType: websocket.TextMessage, payload: bytes.Repeat([]byte("x"), remoteCaptchaMaxInputPayloadSize+1)},
		{name: "binary input", messageType: websocket.BinaryMessage, payload: []byte(`{"type":"pointer"}`)},
		{name: "rejected typed input", messageType: websocket.TextMessage, payload: []byte(`{"type":"keyboard","key":"Enter"}`), dispatchErr: errRemoteCaptchaInputInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRemoteCaptchaWebSocketFixture(t)
			fixture.stream.dispatchErr = test.dispatchErr
			connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
			if err != nil {
				t.Fatalf("dial response=%v err=%v", response, err)
			}
			select {
			case <-fixture.streamOwners:
			case <-time.After(time.Second):
				t.Fatal("remote stream did not start")
			}
			if err := connection.WriteMessage(test.messageType, test.payload); err != nil {
				t.Fatal(err)
			}
			_ = connection.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := connection.ReadMessage(); err == nil {
				t.Fatal("protocol violation left WebSocket open")
			}
			_ = connection.Close()
			select {
			case <-fixture.browser.closed:
			case <-time.After(time.Second):
				t.Fatal("protocol violation did not close browser")
			}
			select {
			case <-fixture.stream.done:
			case <-time.After(time.Second):
				t.Fatal("protocol violation did not close stream")
			}
			fixture.server.mu.Lock()
			_, live := fixture.server.passwordPending[fixture.state]
			fixture.server.mu.Unlock()
			if live {
				t.Fatal("protocol violation retained password state")
			}
			select {
			case duration := <-fixture.graceDurations:
				t.Fatalf("protocol violation started reconnect grace %s", duration)
			default:
			}
		})
	}
}

func TestRemoteCaptchaWebSocketInputRateAndBackpressureAreNonTerminal(t *testing.T) {
	for _, dispatchErr := range []error{errRemoteCaptchaInputRate, errRemoteCaptchaInputBusy} {
		t.Run(dispatchErr.Error(), func(t *testing.T) {
			fixture := newRemoteCaptchaWebSocketFixture(t)
			fixture.stream.dispatchErr = dispatchErr
			connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
			if err != nil {
				t.Fatalf("dial response=%v err=%v", response, err)
			}
			select {
			case <-fixture.streamOwners:
			case <-time.After(time.Second):
				t.Fatal("remote stream did not start")
			}
			input := []byte(`{"type":"pointer","phase":"move","x":320,"y":225,"width":640,"height":450,"button":0}`)
			if err := connection.WriteMessage(websocket.TextMessage, input); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-fixture.stream.dispatches:
				if !bytes.Equal(got, input) {
					t.Fatalf("dispatch payload=%q, want %q", got, input)
				}
			case <-time.After(time.Second):
				t.Fatal("pointer move was not dispatched")
			}
			select {
			case <-fixture.browser.closed:
				t.Fatalf("transient input error %v closed the browser", dispatchErr)
			case <-time.After(50 * time.Millisecond):
			}
			fixture.server.mu.Lock()
			_, live := fixture.server.passwordPending[fixture.state]
			fixture.server.mu.Unlock()
			if !live {
				t.Fatalf("transient input error %v deleted the flow", dispatchErr)
			}

			if err := connection.WriteMessage(websocket.BinaryMessage, []byte("malformed")); err != nil {
				t.Fatal(err)
			}
			_ = connection.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := connection.ReadMessage(); err == nil {
				t.Fatal("definite protocol violation left WebSocket open")
			}
			_ = connection.Close()
			select {
			case <-fixture.browser.closed:
			case <-time.After(time.Second):
				t.Fatal("definite protocol violation did not close browser")
			}
		})
	}
}

func TestRemoteCaptchaWebSocketClassifiesDefiniteProtocolReadErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "read limit", err: websocket.ErrReadLimit, want: true},
		{name: "malformed frame", err: errors.New("websocket: unexpected reserved bits 0x4"), want: true},
		{name: "protocol close", err: &websocket.CloseError{Code: websocket.CloseProtocolError}, want: true},
		{name: "unsupported data", err: &websocket.CloseError{Code: websocket.CloseUnsupportedData}, want: true},
		{name: "invalid payload", err: &websocket.CloseError{Code: websocket.CloseInvalidFramePayloadData}, want: true},
		{name: "policy violation", err: &websocket.CloseError{Code: websocket.ClosePolicyViolation}, want: true},
		{name: "too big close", err: &websocket.CloseError{Code: websocket.CloseMessageTooBig}, want: true},
		{name: "normal close", err: &websocket.CloseError{Code: websocket.CloseNormalClosure}, want: false},
		{name: "going away", err: &websocket.CloseError{Code: websocket.CloseGoingAway}, want: false},
		{name: "network loss", err: &websocket.CloseError{Code: websocket.CloseAbnormalClosure}, want: false},
		{name: "transport eof", err: io.EOF, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := remoteCaptchaProtocolReadViolation(test.err); got != test.want {
				t.Fatalf("protocol violation=%v, want %v for %T: %v", got, test.want, test.err, test.err)
			}
		})
	}
}

func TestRemoteCaptchaWebSocketViewerCancellationClosesSocketStreamAndBrowser(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		t.Fatalf("dial response=%v err=%v", response, err)
	}
	select {
	case <-fixture.streamOwners:
	case <-time.After(time.Second):
		t.Fatal("remote stream did not start")
	}

	cancelRequest := httptest.NewRequest(http.MethodPost, fixture.origin+"/api/auth/captcha/remote/cancel", strings.NewReader(`{}`))
	cancelRequest.Host = strings.TrimPrefix(fixture.origin, "https://")
	cancelRequest.Header.Set("Origin", fixture.origin)
	cancelRequest.Header.Set("Content-Type", "application/json")
	cancelRequest.AddCookie(fixture.cookie)
	cancelResponse := serveRemoteCaptchaHTTP(fixture.server, cancelRequest)
	if cancelResponse.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%q", cancelResponse.Code, cancelResponse.Body.String())
	}

	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("viewer cancellation left WebSocket open")
	}
	_ = connection.Close()
	select {
	case <-fixture.stream.done:
	case <-time.After(time.Second):
		t.Fatal("viewer cancellation did not close stream")
	}
	select {
	case <-fixture.browser.closed:
	case <-time.After(time.Second):
		t.Fatal("viewer cancellation did not close browser")
	}
}

func TestRemoteCaptchaWebSocketWriterUsesBoundedUpstreamAndOneWriter(t *testing.T) {
	stream := newTestRemoteCaptchaStream()
	writer := newBlockingRemoteCaptchaWebSocketWriter()
	ping := make(chan time.Time, 1)
	now := time.Unix(1_800_000_000, 0)
	hooks := remoteCaptchaWebSocketTiming{
		now: func() time.Time { return now },
		after: func(duration time.Duration) <-chan time.Time {
			if duration != remoteCaptchaWebSocketPingPeriod {
				t.Errorf("ping period=%s", duration)
			}
			return ping
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writeRemoteCaptchaWebSocket(ctx, writer, stream, hooks) }()

	stream.frames <- []byte("frame-1")
	select {
	case <-writer.firstWriteStarted:
	case <-time.After(time.Second):
		t.Fatal("writer did not start first frame")
	}
	stream.frames <- []byte("frame-2")
	select {
	case stream.frames <- []byte("frame-3"):
		t.Fatal("WebSocket writer prefetched beyond the upstream size-one frame queue")
	default:
	}
	close(writer.releaseFirstWrite)

	deadline := time.Now().Add(time.Second)
	for {
		writer.mu.Lock()
		frames := len(writer.frames)
		writer.mu.Unlock()
		if frames == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer did not deliver retained newest frame")
		}
		time.Sleep(time.Millisecond)
	}
	ping <- now
	deadline = time.Now().Add(time.Second)
	for {
		writer.mu.Lock()
		controls := append([]int(nil), writer.controls...)
		writer.mu.Unlock()
		if len(controls) > 0 && controls[0] == websocket.PingMessage {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer did not send ping")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer did not stop")
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.maxActiveWrites != 1 {
		t.Fatalf("concurrent WebSocket writes=%d, want 1", writer.maxActiveWrites)
	}
	if len(writer.controls) < 2 || writer.controls[len(writer.controls)-1] != websocket.CloseMessage {
		t.Fatalf("control messages=%v, want ping then close", writer.controls)
	}
	if len(writer.deadlines) < 4 {
		t.Fatalf("write deadlines=%d, want each frame/control bounded", len(writer.deadlines))
	}
}

func TestRemoteCaptchaWebSocketWaitsForControllerSessionWithoutRelaunch(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	readyForRetry := make(chan time.Time, 1)
	var attempts atomic.Int32
	fixture.server.remoteCaptchaStreamRetryAfter = func(duration time.Duration) <-chan time.Time {
		if duration != remoteCaptchaStreamAttachRetryInterval {
			t.Errorf("stream attach retry=%s", duration)
		}
		return readyForRetry
	}
	fixture.server.remoteCaptchaStartStream = func(controller captchaBrowserController, flowCtx, processCtx, viewerCtx, serverCtx context.Context) (remoteCaptchaStreamSession, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("remote CAPTCHA Chrome session is unavailable")
		}
		fixture.streamCalls.Add(1)
		fixture.streamOwners <- [4]context.Context{flowCtx, processCtx, viewerCtx, serverCtx}
		return fixture.stream, nil
	}

	connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		t.Fatalf("dial response=%v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	deadline := time.Now().Add(time.Second)
	for attempts.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("initial stream attach attempt missing")
		}
		time.Sleep(time.Millisecond)
	}
	if got := fixture.launchCalls.Load(); got != 1 {
		t.Fatalf("initial browser launches=%d, want 1", got)
	}
	readyForRetry <- time.Now()
	select {
	case <-fixture.streamOwners:
	case <-time.After(time.Second):
		t.Fatal("stream did not retry after the controller session became ready")
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("stream attach attempts=%d, want 2", got)
	}
	if got := fixture.launchCalls.Load(); got != 1 {
		t.Fatalf("stream attach retry relaunched browser %d times", got)
	}
}

func TestRemoteCaptchaWebSocketReadLimitAndPongRefreshAreBounded(t *testing.T) {
	connection := newObservedRemoteCaptchaWebSocketConnection()
	stream := newTestRemoteCaptchaStream()
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	relay := &remoteCaptchaRelay{ctx: relayCtx, stream: stream}
	current := time.Unix(1_800_000_000, 0)
	neverPing := make(chan time.Time)
	timing := remoteCaptchaWebSocketTiming{
		now:   func() time.Time { return current },
		after: func(time.Duration) <-chan time.Time { return neverPing },
	}
	done := make(chan bool, 1)
	go func() { done <- (&Server{}).serveRemoteCaptchaWebSocket(connection, relay, timing) }()

	deadline := time.Now().Add(time.Second)
	var pong func(string) error
	for pong == nil {
		connection.mu.Lock()
		pong = connection.pongHandler
		connection.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("pong handler was not installed")
		}
		time.Sleep(time.Millisecond)
	}
	connection.mu.Lock()
	if connection.readLimit != remoteCaptchaMaxInputPayloadSize {
		t.Fatalf("read limit=%d, want %d", connection.readLimit, remoteCaptchaMaxInputPayloadSize)
	}
	if len(connection.readDeadlines) != 1 || !connection.readDeadlines[0].Equal(current.Add(remoteCaptchaWebSocketPongWait)) {
		t.Fatalf("initial read deadlines=%v", connection.readDeadlines)
	}
	connection.mu.Unlock()

	current = current.Add(7 * time.Second)
	if err := pong("alive"); err != nil {
		t.Fatal(err)
	}
	connection.mu.Lock()
	if len(connection.readDeadlines) != 2 || !connection.readDeadlines[1].Equal(current.Add(remoteCaptchaWebSocketPongWait)) {
		t.Fatalf("pong read deadlines=%v", connection.readDeadlines)
	}
	connection.mu.Unlock()
	cancelRelay()
	select {
	case violation := <-done:
		if violation {
			t.Fatal("terminal cancellation was classified as a protocol violation")
		}
	case <-time.After(time.Second):
		t.Fatal("connection did not stop after viewer cancellation")
	}
}

func TestRemoteCaptchaWebSocketBrowserExitClosesViewerAndPublishesFailure(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		t.Fatalf("dial response=%v err=%v", response, err)
	}
	select {
	case <-fixture.streamOwners:
	case <-time.After(time.Second):
		t.Fatal("remote stream did not start")
	}
	fixture.browser.exitOnce.Do(func() { close(fixture.browser.processDone) })
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("browser exit left WebSocket open")
	}
	_ = connection.Close()
	select {
	case <-fixture.stream.done:
	case <-time.After(time.Second):
		t.Fatal("browser exit did not close stream")
	}
	select {
	case <-fixture.browser.closed:
	case <-time.After(time.Second):
		t.Fatal("browser exit did not finalize controller cleanup")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _, waitErr := fixture.server.WaitPasswordLogin(waitCtx, fixture.state)
	if waitErr == nil || !strings.Contains(waitErr.Error(), "browser exited") {
		t.Fatalf("browser exit outcome=%v", waitErr)
	}
}

func TestRemoteCaptchaWebSocketRiotCompletionClosesViewerAndContinuesMFAOrPersistence(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*fakeBrowserPasswordAuth)
		wantMFA     bool
		wantDisplay string
	}{
		{
			name: "MFA",
			configure: func(auth *fakeBrowserPasswordAuth) {
				auth.mfa = &riot.MFAChallenge{Email: "r***@example.com", Method: "email"}
			},
			wantMFA: true,
		},
		{
			name: "success",
			configure: func(auth *fakeBrowserPasswordAuth) {
				auth.tokens = riot.PasswordTokens{
					AccessToken:   jwtAccessToken("ap"),
					IDToken:       jwtIDToken("Remote", "AP1"),
					SessionCookie: "ssid=remote-session",
				}
			},
			wantDisplay: "Remote#AP1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRemoteCaptchaWebSocketFixture(t)
			auth := fixture.server.passwordAuth.(*fakeBrowserPasswordAuth)
			test.configure(auth)
			releaseResult := make(chan struct{})
			fixture.browser.run = func(ctx context.Context, _, _ string) (riotBrowserLoginResult, error) {
				select {
				case <-releaseResult:
					return riotBrowserLoginResult{ResponseBody: []byte(`{"type":"complete"}`), UserAgent: "remote-browser/1"}, nil
				case <-ctx.Done():
					return riotBrowserLoginResult{}, ctx.Err()
				}
			}
			connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
			if err != nil {
				t.Fatalf("dial response=%v err=%v", response, err)
			}
			select {
			case <-fixture.streamOwners:
			case <-time.After(time.Second):
				t.Fatal("remote stream did not start")
			}
			close(releaseResult)
			_ = connection.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := connection.ReadMessage(); err == nil {
				t.Fatal("Riot completion left WebSocket open")
			}
			_ = connection.Close()
			select {
			case <-fixture.stream.done:
			case <-time.After(time.Second):
				t.Fatal("Riot completion did not close stream")
			}
			select {
			case <-fixture.browser.closed:
			case <-time.After(time.Second):
				t.Fatal("Riot completion did not close browser")
			}

			waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			display, mfaState, _, waitErr := fixture.server.WaitPasswordLogin(waitCtx, fixture.state)
			cancel()
			if waitErr != nil {
				t.Fatal(waitErr)
			}
			fixture.server.mu.Lock()
			_, passwordStateLive := fixture.server.passwordPending[fixture.state]
			fixture.server.mu.Unlock()
			if passwordStateLive {
				t.Fatal("Riot completion retained remote password/viewer state")
			}
			if test.wantMFA {
				if mfaState == "" || display != "" {
					t.Fatalf("MFA continuation display=%q state=%q", display, mfaState)
				}
				return
			}
			if mfaState != "" || display != test.wantDisplay {
				t.Fatalf("success display=%q MFA=%q", display, mfaState)
			}
			if len(fixture.store.accounts) != 1 || fixture.store.accounts[0].DiscordUserID != "discord-owner" {
				t.Fatalf("stored accounts=%+v", fixture.store.accounts)
			}
			if string(fixture.boxer.lastPlain) != "ssid=remote-session" {
				t.Fatalf("persisted session plaintext=%q", fixture.boxer.lastPlain)
			}
		})
	}
}

func TestRemoteCaptchaWebSocketUnexpectedStreamEndClosesViewerBrowserAndFlow(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		t.Fatalf("dial response=%v err=%v", response, err)
	}
	select {
	case <-fixture.streamOwners:
	case <-time.After(time.Second):
		t.Fatal("remote stream did not start")
	}
	if err := fixture.stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("unexpected stream end left WebSocket open")
	}
	_ = connection.Close()
	select {
	case <-fixture.browser.closed:
	case <-time.After(time.Second):
		t.Fatal("unexpected stream end did not close browser")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _, waitErr := fixture.server.WaitPasswordLogin(waitCtx, fixture.state)
	if waitErr == nil || !strings.Contains(waitErr.Error(), "stream ended unexpectedly") {
		t.Fatalf("unexpected stream outcome=%v", waitErr)
	}
	fixture.server.mu.Lock()
	_, live := fixture.server.passwordPending[fixture.state]
	fixture.server.mu.Unlock()
	if live {
		t.Fatal("unexpected stream end retained remote password/viewer state")
	}
}

func TestRemoteCaptchaWebSocketShutdownClosesConnectionAndDrainsStreamWorker(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	closeStarted := make(chan struct{})
	closeRelease := make(chan struct{})
	fixture.stream.closeStarted = closeStarted
	fixture.stream.closeRelease = closeRelease
	connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		t.Fatalf("dial response=%v err=%v", response, err)
	}
	select {
	case <-fixture.streamOwners:
	case <-time.After(time.Second):
		t.Fatal("remote stream did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- fixture.server.Shutdown(context.Background()) }()
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not request stream close")
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before stream worker drained: %v", err)
	default:
	}
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("shutdown left WebSocket open")
	}
	_ = connection.Close()
	close(closeRelease)
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain WebSocket and stream workers")
	}
	select {
	case <-fixture.browser.closed:
	default:
		t.Fatal("shutdown retained remote browser")
	}
}

func TestRemoteCaptchaWebSocketOwnerCancelClosesConnectionStreamAndBrowser(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		t.Fatalf("dial response=%v err=%v", response, err)
	}
	select {
	case <-fixture.streamOwners:
	case <-time.After(time.Second):
		t.Fatal("remote stream did not start")
	}
	if err := fixture.server.CancelPasswordLogin(fixture.state, "discord-owner"); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("owner cancellation left WebSocket open")
	}
	_ = connection.Close()
	select {
	case <-fixture.stream.done:
	case <-time.After(time.Second):
		t.Fatal("owner cancellation did not close stream")
	}
	select {
	case <-fixture.browser.closed:
	case <-time.After(time.Second):
		t.Fatal("owner cancellation did not close browser")
	}
}

func TestRemoteCaptchaWebSocketOverallTTLClosesActiveViewerWithoutExtension(t *testing.T) {
	fixture := newRemoteCaptchaWebSocketFixture(t)
	connection, response, err := fixture.dial(t, fixture.cookie, fixture.origin)
	if err != nil {
		t.Fatalf("dial response=%v err=%v", response, err)
	}
	select {
	case <-fixture.streamOwners:
	case <-time.After(time.Second):
		t.Fatal("remote stream did not start")
	}
	fixture.server.mu.Lock()
	originalExpiry := fixture.server.passwordPending[fixture.state].remoteViewer.expiresAt
	fixture.server.mu.Unlock()
	fixture.clock.Advance(time.Minute)
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("remote TTL expiry left WebSocket open")
	}
	_ = connection.Close()
	select {
	case <-fixture.stream.done:
	case <-time.After(time.Second):
		t.Fatal("remote TTL expiry did not close stream")
	}
	select {
	case <-fixture.browser.closed:
	case <-time.After(time.Second):
		t.Fatal("remote TTL expiry did not close browser")
	}
	fixture.server.mu.Lock()
	pending, live := fixture.server.passwordPending[fixture.state]
	fixture.server.mu.Unlock()
	if live {
		t.Fatalf("remote TTL expiry retained state with expiry %s (original %s)", pending.remoteViewer.expiresAt, originalExpiry)
	}
}
