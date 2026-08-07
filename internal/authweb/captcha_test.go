package authweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

func TestMain(m *testing.M) {
	skipCaptchaTLSWait = true
	dir, err := os.MkdirTemp("", "captcha-chrome-test-*")
	if err != nil {
		panic(err)
	}
	chromeUserDataDirFn = func() (string, error) { return dir, nil }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type fakePasswordAuth struct {
	ch              riot.CaptchaChallenge
	tokens          riot.PasswordTokens
	mfa             *riot.MFAChallenge
	complete        error
	completeCalls   atomic.Int32
	beginCalls      atomic.Int32
	retryOnce       bool
	retryCookies    []*http.Cookie
	beginDelay      time.Duration
	completeStarted chan struct{}
	completeRelease <-chan struct{}
	completeSession atomic.Value
	beginBrowser    atomic.Value
	completeBrowser atomic.Value
	canceledSession atomic.Value
	mfaCalls        atomic.Int32
	mfaStarted      chan struct{}
	mfaRelease      <-chan struct{}
	mfaErr          error
}

type testCaptchaBrowserController struct {
	closeOnce    sync.Once
	closeStarted chan struct{}
	closeRelease <-chan struct{}
	closed       chan struct{}
	onClose      func()
	closeErr     error
	closeCalls   atomic.Int32
}

type blockingCaptchaStore struct {
	*mockStore
	upsertStarted chan struct{}
	upsertRelease <-chan struct{}
}

type cancelBlockingRiot struct {
	*mockRiot
	namesStarted chan struct{}
	namesRelease <-chan struct{}
}

type callbackCanceledContext struct {
	context.Context
	done    chan struct{}
	onErr   func()
	errOnce sync.Once
}

func newCallbackCanceledContext(onErr func()) *callbackCanceledContext {
	done := make(chan struct{})
	close(done)
	return &callbackCanceledContext{
		Context: context.Background(),
		done:    done,
		onErr:   onErr,
	}
}

func (c *callbackCanceledContext) Done() <-chan struct{} {
	return c.done
}

func (c *callbackCanceledContext) Err() error {
	c.errOnce.Do(c.onErr)
	return context.Canceled
}

func (r *cancelBlockingRiot) GetPlayerNames(ctx context.Context, accessToken, entitlementsToken, shard string, puuids []string) ([]riot.PlayerName, error) {
	select {
	case r.namesStarted <- struct{}{}:
	default:
	}
	select {
	case <-r.namesRelease:
		return r.mockRiot.GetPlayerNames(ctx, accessToken, entitlementsToken, shard, puuids)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *blockingCaptchaStore) UpsertRiotAccount(account store.Account) error {
	select {
	case s.upsertStarted <- struct{}{}:
	default:
	}
	if s.upsertRelease != nil {
		<-s.upsertRelease
	}
	return s.mockStore.UpsertRiotAccount(account)
}

func newTestCaptchaBrowserController() *testCaptchaBrowserController {
	return &testCaptchaBrowserController{
		closeStarted: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (c *testCaptchaBrowserController) Close() error {
	c.closeOnce.Do(func() {
		c.closeCalls.Add(1)
		close(c.closeStarted)
		if c.onClose != nil {
			c.onClose()
		}
		if c.closeRelease != nil {
			<-c.closeRelease
		}
		close(c.closed)
	})
	return c.closeErr
}

func (f *fakePasswordAuth) BeginCaptcha(ctx context.Context, username, password string, browser riot.CaptchaBrowserSession) (riot.CaptchaChallenge, error) {
	f.beginCalls.Add(1)
	f.beginBrowser.Store(browser)
	if f.beginDelay > 0 {
		select {
		case <-time.After(f.beginDelay):
		case <-ctx.Done():
			return riot.CaptchaChallenge{}, ctx.Err()
		}
	}
	return f.ch, nil
}

func (f *fakePasswordAuth) CompleteCaptcha(ctx context.Context, sessionID, captchaToken string, browser riot.CaptchaBrowserSession) (riot.PasswordTokens, *riot.MFAChallenge, error) {
	f.completeSession.Store(sessionID)
	f.completeBrowser.Store(browser)
	if f.completeStarted != nil {
		select {
		case f.completeStarted <- struct{}{}:
		default:
		}
	}
	if f.completeRelease != nil {
		<-f.completeRelease
	}
	n := f.completeCalls.Add(1)
	if f.retryOnce && n == 1 {
		cookies := f.retryCookies
		if cookies == nil {
			cookies = []*http.Cookie{
				{Name: "authenticator.sid", Value: "retry-session", Path: "/", Secure: true, HttpOnly: true},
			}
		}
		return riot.PasswordTokens{}, nil, &riot.CaptchaRetryError{
			SiteKey:        "k-retry",
			RQData:         "d-retry",
			Reason:         "captcha_not_allowed",
			BrowserCookies: cookies,
		}
	}
	if f.complete != nil {
		return riot.PasswordTokens{}, nil, f.complete
	}
	return f.tokens, f.mfa, nil
}

func testCaptchaBrowserSession() riot.CaptchaBrowserSession {
	return riot.CaptchaBrowserSession{UserAgent: "captcha-browser/1"}
}

func newCaptchaRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = RiotCaptchaHost
	req.Header.Set("User-Agent", "captcha-browser/1")
	if method == http.MethodPost || method == http.MethodOptions {
		req.Header.Set("Origin", "https://"+RiotCaptchaHost)
	}
	return req
}

func TestCaptchaChallengeSyncsRiotBrowserSession(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{
			SessionID: "session-browser",
			SiteKey:   "site-key",
			RQData:    "rq-data",
			BrowserCookies: []*http.Cookie{
				{Name: "authenticator.sid", Value: "s1", Path: "/", Secure: true, HttpOnly: true},
				{Name: "tdid", Value: "d1", Domain: "riotgames.com", Path: "/", Secure: true, HttpOnly: true},
			},
		},
		mfa: &riot.MFAChallenge{Email: "a***@example.com", Method: "email"},
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "discord-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}

	challengeReq := httptest.NewRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
	challengeReq.Host = RiotCaptchaHost
	challengeReq.Header.Set("User-Agent", "captcha-browser/1")
	challengeRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(challengeRec, challengeReq)
	if challengeRec.Code != http.StatusOK {
		t.Fatalf("challenge status=%d body=%s", challengeRec.Code, challengeRec.Body.String())
	}
	beginBrowser, _ := pw.beginBrowser.Load().(riot.CaptchaBrowserSession)
	if beginBrowser.UserAgent != "captcha-browser/1" {
		t.Fatalf("begin browser = %#v", beginBrowser)
	}

	responseCookies := challengeRec.Result().Cookies()
	values := map[string]string{}
	for _, cookie := range responseCookies {
		values[cookie.Name] = cookie.Value
		if !cookie.Secure || !cookie.HttpOnly {
			t.Fatalf("cookie %s flags: Secure=%v HttpOnly=%v", cookie.Name, cookie.Secure, cookie.HttpOnly)
		}
	}
	if values["authenticator.sid"] != "s1" || values["tdid"] != "d1" {
		t.Fatalf("challenge cookies = %#v", values)
	}

	submitReq := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
	submitReq.Host = RiotCaptchaHost
	submitReq.Header.Set("Origin", "https://"+RiotCaptchaHost)
	submitReq.Header.Set("User-Agent", "captcha-browser/1")
	submitReq.Header.Set("Content-Type", "application/json")
	for _, cookie := range responseCookies {
		submitReq.AddCookie(cookie)
	}
	submitRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(submitRec, submitReq)
	if submitRec.Code != http.StatusOK {
		t.Fatalf("submit status=%d body=%s", submitRec.Code, submitRec.Body.String())
	}
	completeBrowser, _ := pw.completeBrowser.Load().(riot.CaptchaBrowserSession)
	if completeBrowser.UserAgent != "captcha-browser/1" ||
		completeBrowser.Cookies["authenticator.sid"] != "s1" ||
		completeBrowser.Cookies["tdid"] != "d1" {
		t.Fatalf("complete browser = %#v", completeBrowser)
	}
}

func TestCaptchaSubmitDoesNotRecreateCanceledState(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	pw := &fakePasswordAuth{
		ch:              riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		retryOnce:       true,
		completeStarted: started,
		completeRelease: release,
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
		req.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rec, req)
	}()
	<-started
	cleanupDone := make(chan struct{})
	go func() {
		s.cleanupPasswordState(state)
		close(cleanupDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		_, stillPending := s.passwordPending[state]
		s.mu.Unlock()
		if !stillPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not remove pending state")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	<-done
	<-cleanupDone

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.Lock()
	_, pending := s.passwordPending[state]
	_, outcome := s.passwordOutcomes[state]
	s.mu.Unlock()
	if pending || outcome {
		t.Fatalf("canceled state was recreated: pending=%v outcome=%v", pending, outcome)
	}
}

func TestPublicCaptchaLaunchRoutesAreNotMounted(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/captcha?state=secret"},
		{http.MethodGet, "/captcha/open?state=secret"},
		{http.MethodPost, "/api/auth/captcha/launch"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s status=%d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

func TestCaptchaEndpointsRejectWrongHostOrOrigin(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}

	wrongHost := httptest.NewRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
	wrongHost.Host = "localhost"
	wrongHost.Header.Set("User-Agent", "captcha-browser/1")
	wrongHostRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(wrongHostRec, wrongHost)
	if wrongHostRec.Code != http.StatusBadRequest || pw.beginCalls.Load() != 0 {
		t.Fatalf("wrong-host status=%d beginCalls=%d", wrongHostRec.Code, pw.beginCalls.Load())
	}

	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}
	wrongOrigin := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
	wrongOrigin.Host = RiotCaptchaHost
	wrongOrigin.Header.Set("Origin", "https://attacker.example")
	wrongOrigin.Header.Set("User-Agent", "captcha-browser/1")
	wrongOrigin.Header.Set("Content-Type", "application/json")
	wrongOriginRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(wrongOriginRec, wrongOrigin)
	if wrongOriginRec.Code != http.StatusForbidden || pw.completeCalls.Load() != 0 {
		t.Fatalf("wrong-origin status=%d completeCalls=%d", wrongOriginRec.Code, pw.completeCalls.Load())
	}
}

func TestCaptchaChallengeInitializationIsSingleFlight(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{
			SessionID: "session-from-widget",
			SiteKey:   "site-key",
			RQData:    "rq-data",
		},
		tokens: riot.PasswordTokens{
			AccessToken:   "at-1",
			IDToken:       "id-1",
			SessionCookie: "ssid=s1",
		},
		beginDelay: 100 * time.Millisecond,
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "discord-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}

	const callers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			pending, challengeErr := s.ensureCaptchaChallenge(ctx, state, testCaptchaBrowserSession())
			if challengeErr != nil {
				t.Errorf("challenge: %v", challengeErr)
				return
			}
			if pending.sessionID != "session-from-widget" || pending.rqData != "rq-data" {
				t.Errorf("challenge/session mismatch: %+v", pending)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := pw.beginCalls.Load(); got != 1 {
		t.Fatalf("BeginCaptcha calls = %d, want exactly 1 for one Discord state", got)
	}

	subReq := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"hcaptcha-token","version":1}`))
	subReq.Header.Set("Content-Type", "application/json")
	subRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(subRec, subReq)
	if subRec.Code != http.StatusOK {
		t.Fatalf("submit %d %s", subRec.Code, subRec.Body.String())
	}
	if got, _ := pw.completeSession.Load().(string); got != "session-from-widget" {
		t.Fatalf("CompleteCaptcha session = %q, want browser challenge session", got)
	}
}

func TestCaptchaSubmissionsAreSerializedAndSingleUse(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		tokens: riot.PasswordTokens{
			AccessToken:   "at-1",
			IDToken:       "id-1",
			SessionCookie: "ssid=s1",
		},
		completeStarted: started,
		completeRelease: release,
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}

	request := func(rec *httptest.ResponseRecorder, done chan<- struct{}) {
		defer close(done)
		req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
		req.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rec, req)
	}
	rec1, rec2 := httptest.NewRecorder(), httptest.NewRecorder()
	done1, done2 := make(chan struct{}), make(chan struct{})
	go request(rec1, done1)
	<-started
	go request(rec2, done2)

	secondReachedRiot := false
	select {
	case <-started:
		secondReachedRiot = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-done1
	<-done2

	if secondReachedRiot || pw.completeCalls.Load() != 1 {
		t.Fatalf("concurrent solves reached Riot: second=%v calls=%d", secondReachedRiot, pw.completeCalls.Load())
	}
	if rec1.Code != http.StatusOK || rec2.Code == http.StatusOK {
		t.Fatalf("responses: first=%d %s second=%d %s", rec1.Code, rec1.Body.String(), rec2.Code, rec2.Body.String())
	}
}

func TestMFASubmissionIsSerializedAndSingleUse(t *testing.T) {
	mfaStarted := make(chan struct{}, 2)
	mfaRelease := make(chan struct{})
	pw := &fakePasswordAuth{
		ch:  riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa: &riot.MFAChallenge{Email: "a***@ex.com", Method: "email"},
		tokens: riot.PasswordTokens{
			AccessToken:   "at-1",
			IDToken:       "id-1",
			SessionCookie: "ssid=s1",
		},
		mfaStarted: mfaStarted,
		mfaRelease: mfaRelease,
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}
	req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("captcha submit: %d %s", rec.Code, rec.Body.String())
	}
	_, mfaState, _, err := s.WaitPasswordLogin(context.Background(), state)
	if err != nil || mfaState == "" {
		t.Fatalf("mfa state=%q err=%v", mfaState, err)
	}

	type result struct {
		display string
		err     error
	}
	results := make(chan result, 2)
	go func() {
		display, submitErr := s.CompletePasswordMFA(context.Background(), mfaState, "owner-1", "123456")
		results <- result{display: display, err: submitErr}
	}()
	<-mfaStarted
	go func() {
		display, submitErr := s.CompletePasswordMFA(context.Background(), mfaState, "owner-1", "123456")
		results <- result{display: display, err: submitErr}
	}()

	secondReachedRiot := false
	select {
	case <-mfaStarted:
		secondReachedRiot = true
	case <-time.After(100 * time.Millisecond):
	}
	close(mfaRelease)
	r1, r2 := <-results, <-results

	if secondReachedRiot || pw.mfaCalls.Load() != 1 {
		t.Fatalf("concurrent MFA reached Riot: second=%v calls=%d", secondReachedRiot, pw.mfaCalls.Load())
	}
	successes := 0
	for _, result := range []result{r1, r2} {
		if result.err == nil && result.display != "" {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("MFA successes=%d results=%+v %+v", successes, r1, r2)
	}
}

func (f *fakePasswordAuth) SubmitMFA(ctx context.Context, challenge *riot.MFAChallenge, code string) (riot.PasswordTokens, error) {
	f.mfaCalls.Add(1)
	if f.mfaStarted != nil {
		f.mfaStarted <- struct{}{}
	}
	if f.mfaRelease != nil {
		select {
		case <-f.mfaRelease:
		case <-ctx.Done():
			return riot.PasswordTokens{}, ctx.Err()
		}
	}
	if f.mfaErr != nil {
		return riot.PasswordTokens{}, f.mfaErr
	}
	return f.tokens, nil
}

func (f *fakePasswordAuth) CancelCaptcha(sessionID string) {
	f.canceledSession.Store(sessionID)
}

func newCaptchaServer(pw *fakePasswordAuth) *Server {
	s := New(Deps{
		AuthBaseURL:  "https://bot.example.com",
		PasswordAuth: pw,
		PendingTTL:   time.Minute,
		Store:        newMockStore(),
		Riot: &mockRiot{
			entitlements: "ent",
			puuid:        "puuid-1",
			names:        []riot.PlayerName{{GameName: "Player", TagLine: "KR1"}},
			region:       "kr",
			shard:        "kr",
		},
		Boxer: &mockBoxer{},
	})
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		return newTestCaptchaBrowserController(), nil
	}
	return s
}

func createMFAStateThroughCaptcha(t *testing.T, s *Server, discordUserID string) string {
	t.Helper()
	_, state, err := s.BeginPasswordLogin(context.Background(), discordUserID, "riot-user", "secret-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}
	req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("captcha submit: %d %s", rec.Code, rec.Body.String())
	}
	_, mfaState, _, err := s.WaitPasswordLogin(context.Background(), state)
	if err != nil || mfaState == "" {
		t.Fatalf("MFA transition state=%q err=%v", mfaState, err)
	}
	return mfaState
}

func TestMFAOwnerValidation(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:  riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa: &riot.MFAChallenge{Email: "a***@ex.com", Method: "email"},
	}
	s := newCaptchaServer(pw)
	mfaState := createMFAStateThroughCaptcha(t, s, "owner-1")

	hint, err := s.ValidatePasswordMFA(mfaState, "owner-1")
	if err != nil || hint != "a***@ex.com" {
		t.Fatalf("owner validation hint=%q err=%v", hint, err)
	}
	if _, err := s.ValidatePasswordMFA(mfaState, "intruder-1"); !errors.Is(err, ErrMFAOwner) {
		t.Fatalf("wrong-owner validation error=%v, want ErrMFAOwner", err)
	}
	if calls := pw.mfaCalls.Load(); calls != 0 {
		t.Fatalf("owner validation reached Riot %d time(s)", calls)
	}
}

func TestMFAOwnerSubmissionRejectsWrongUserBeforeRiot(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:  riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa: &riot.MFAChallenge{Email: "a***@ex.com", Method: "email"},
	}
	s := newCaptchaServer(pw)
	mfaState := createMFAStateThroughCaptcha(t, s, "owner-1")

	if _, err := s.CompletePasswordMFA(context.Background(), mfaState, "intruder-1", "123456"); !errors.Is(err, ErrMFAOwner) {
		t.Fatalf("wrong-owner submission error=%v, want ErrMFAOwner", err)
	}
	if calls := pw.mfaCalls.Load(); calls != 0 {
		t.Fatalf("wrong-owner submission reached Riot %d time(s)", calls)
	}
	if _, err := s.ValidatePasswordMFA(mfaState, "owner-1"); err != nil {
		t.Fatalf("wrong-owner attempt consumed owner state: %v", err)
	}
}

func TestMFASubmissionRejectsExpiredStateBeforeRiot(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:  riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa: &riot.MFAChallenge{Email: "a***@ex.com", Method: "email"},
	}
	s := newCaptchaServer(pw)
	mfaState := createMFAStateThroughCaptcha(t, s, "owner-1")
	s.mu.Lock()
	pending := s.mfaPending[mfaState]
	pending.expiresAt = time.Now().Add(-time.Second)
	s.mfaPending[mfaState] = pending
	s.mu.Unlock()

	if _, err := s.CompletePasswordMFA(context.Background(), mfaState, "owner-1", "123456"); err == nil {
		t.Fatal("expired MFA submission unexpectedly succeeded")
	}
	if calls := pw.mfaCalls.Load(); calls != 0 {
		t.Fatalf("expired MFA submission reached Riot %d time(s)", calls)
	}
}

func TestMFASubmissionInvalidCodeKeepsOwnerStateRetryable(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:     riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa:    &riot.MFAChallenge{Email: "a***@ex.com", Method: "email"},
		mfaErr: fmt.Errorf("riot response: %w", riot.ErrPasswordInvalidCode),
		tokens: riot.PasswordTokens{AccessToken: "at-1", IDToken: "id-1", SessionCookie: "ssid=s1"},
	}
	s := newCaptchaServer(pw)
	mfaState := createMFAStateThroughCaptcha(t, s, "owner-1")

	if _, err := s.CompletePasswordMFA(context.Background(), mfaState, "owner-1", "000000"); !errors.Is(err, riot.ErrPasswordInvalidCode) {
		t.Fatalf("invalid-code error=%v, want ErrPasswordInvalidCode", err)
	}
	if _, err := s.ValidatePasswordMFA(mfaState, "owner-1"); err != nil {
		t.Fatalf("invalid code consumed retry state: %v", err)
	}
	pw.mfaErr = nil
	display, err := s.CompletePasswordMFA(context.Background(), mfaState, "owner-1", "123456")
	if err != nil || display != "Player#KR1" {
		t.Fatalf("owner retry display=%q err=%v", display, err)
	}
	if calls := pw.mfaCalls.Load(); calls != 2 {
		t.Fatalf("Riot MFA calls=%d, want 2", calls)
	}
}

func TestMFASubmissionTransportFailureConsumesState(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:     riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa:    &riot.MFAChallenge{Email: "a***@ex.com", Method: "email"},
		mfaErr: errors.New("riot transport unavailable"),
	}
	s := newCaptchaServer(pw)
	mfaState := createMFAStateThroughCaptcha(t, s, "owner-1")

	if _, err := s.CompletePasswordMFA(context.Background(), mfaState, "owner-1", "123456"); err == nil {
		t.Fatal("transport-failed MFA submission unexpectedly succeeded")
	}
	if _, err := s.ValidatePasswordMFA(mfaState, "owner-1"); !errors.Is(err, ErrMFAExpired) {
		t.Fatalf("transport failure retained MFA state: %v", err)
	}
	if _, err := s.CompletePasswordMFA(context.Background(), mfaState, "owner-1", "123456"); !errors.Is(err, ErrMFAExpired) {
		t.Fatalf("transport-failed MFA retry error=%v, want ErrMFAExpired", err)
	}
	if calls := pw.mfaCalls.Load(); calls != 1 {
		t.Fatalf("transport retry reached Riot: calls=%d, want 1", calls)
	}
}

type failingMFAStore struct {
	*mockStore
	err     error
	started chan<- struct{}
	release <-chan struct{}
}

func (s *failingMFAStore) UpsertRiotAccount(store.Account) error {
	if s.started != nil {
		s.started <- struct{}{}
	}
	if s.release != nil {
		<-s.release
	}
	return s.err
}

func TestMFAPersistenceFailureConsumesState(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:     riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa:    &riot.MFAChallenge{Email: "a***@ex.com", Method: "email"},
		tokens: riot.PasswordTokens{AccessToken: "at-1", IDToken: "id-1", SessionCookie: "ssid=s1"},
	}
	s := newCaptchaServer(pw)
	persistErr := errors.New("sqlite write failed")
	persistStarted := make(chan struct{}, 1)
	persistRelease := make(chan struct{})
	var releaseOnce sync.Once
	releasePersistence := func() { releaseOnce.Do(func() { close(persistRelease) }) }
	defer releasePersistence()
	s.store = &failingMFAStore{
		mockStore: newMockStore(),
		err:       persistErr,
		started:   persistStarted,
		release:   persistRelease,
	}
	mfaState := createMFAStateThroughCaptcha(t, s, "owner-1")

	completed := make(chan error, 1)
	go func() {
		_, err := s.CompletePasswordMFA(context.Background(), mfaState, "owner-1", "123456")
		completed <- err
	}()
	select {
	case <-persistStarted:
	case <-time.After(time.Second):
		t.Fatal("MFA persistence did not start")
	}
	if _, err := s.ValidatePasswordMFA(mfaState, "owner-1"); !errors.Is(err, ErrMFAExpired) {
		t.Fatalf("MFA state remained live after Riot success reached persistence: %v", err)
	}
	releasePersistence()
	if err := <-completed; !errors.Is(err, persistErr) {
		t.Fatalf("persistence error=%v, want %v", err, persistErr)
	}
	if _, err := s.CompletePasswordMFA(context.Background(), mfaState, "owner-1", "123456"); err == nil {
		t.Fatal("consumed MFA state was retryable after persistence failure")
	}
	if calls := pw.mfaCalls.Load(); calls != 1 {
		t.Fatalf("persistence retry reached Riot: calls=%d, want 1", calls)
	}
}

func TestBeginPasswordLogin_DoesNotExposeLocalhostURL(t *testing.T) {
	s := New(Deps{
		AuthBaseURL:  "http://192.168.0.37:8787",
		PasswordAuth: &fakePasswordAuth{},
		PendingTTL:   time.Minute,
		Store:        newMockStore(),
		Riot:         &mockRiot{},
		Boxer:        &mockBoxer{},
	})
	url, state, err := s.BeginPasswordLogin(context.Background(), "d1", "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	if state == "" {
		t.Fatal("expected state")
	}
	if url != "" {
		t.Fatalf("Discord must use a server-side component, got URL %q", url)
	}
}

func TestBeginPasswordLogin_PublicBaseStillUsesServerSideLaunch(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	url, _, err := s.BeginPasswordLogin(context.Background(), "d1", "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	if url != "" {
		t.Fatalf("Discord must not receive a public or localhost captcha URL, got %q", url)
	}
}

func TestBeginPasswordLogin_DoesNotLaunchChrome(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	launched := make(chan struct{}, 1)
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		launched <- struct{}{}
		return newTestCaptchaBrowserController(), nil
	}

	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	_, pending := s.passwordPending[state]
	s.mu.Unlock()
	if !pending {
		t.Fatal("BeginPasswordLogin did not retain a live pending state")
	}
	select {
	case <-launched:
		t.Fatal("BeginPasswordLogin launched Chrome before the owner clicked the captcha button")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBeginPasswordLoginWaitsForBrowserBeforeRiotSession(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{SessionID: "sess-browser", SiteKey: "site-key", RQData: "rq-data"},
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "d1", "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := pw.beginCalls.Load(); got != 0 {
		t.Fatalf("Riot session began %d times before Chrome supplied its identity", got)
	}

	req := newCaptchaRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("challenge status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := pw.beginCalls.Load(); got != 1 {
		t.Fatalf("Riot session begins = %d, want 1 after browser challenge request", got)
	}
}

func TestLaunchPasswordCaptchaValidatesOwner(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
	}
	s := newCaptchaServer(pw)
	var launches atomic.Int32
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		launches.Add(1)
		return newTestCaptchaBrowserController(), nil
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "intruder"); !errors.Is(err, ErrCaptchaOwner) {
		t.Fatalf("intruder launch error = %v, want ErrCaptchaOwner", err)
	}
	if got := launches.Load(); got != 0 {
		t.Fatalf("wrong-owner Chrome launches = %d, want 0", got)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatalf("owner launch: %v", err)
	}
	if got := launches.Load(); got != 1 {
		t.Fatalf("first owner Chrome launches = %d, want 1", got)
	}
}

func TestCaptchaBrowserOwnerLaunchStoresOneController(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	controller := newTestCaptchaBrowserController()
	var launches atomic.Int32
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		launches.Add(1)
		return controller, nil
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	flow.launchMu.Lock()
	stored := flow.browser
	flow.launchMu.Unlock()
	if launches.Load() != 1 || stored != controller {
		t.Fatalf("launches=%d stored=%T, want one owned controller", launches.Load(), stored)
	}
	if controller.closeCalls.Load() != 0 {
		t.Fatal("fresh browser controller was closed during installation")
	}
}

func TestLaunchPasswordCaptchaReplacesBrowserAfterClosingExisting(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	first := newTestCaptchaBrowserController()
	second := newTestCaptchaBrowserController()
	var eventsMu sync.Mutex
	var events []string
	first.onClose = func() {
		eventsMu.Lock()
		events = append(events, "close:first")
		eventsMu.Unlock()
	}
	var launches atomic.Int32
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		n := launches.Add(1)
		eventsMu.Lock()
		events = append(events, fmt.Sprintf("launch:%d", n))
		eventsMu.Unlock()
		if n == 1 {
			return first, nil
		}
		return second, nil
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}

	eventsMu.Lock()
	gotEvents := append([]string(nil), events...)
	eventsMu.Unlock()
	wantEvents := []string{"launch:1", "close:first", "launch:2"}
	if !slices.Equal(gotEvents, wantEvents) {
		t.Fatalf("events=%v, want %v", gotEvents, wantEvents)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	flow.launchMu.Lock()
	stored := flow.browser
	flow.launchMu.Unlock()
	if stored != second || first.closeCalls.Load() != 1 {
		t.Fatalf("stored=%T first closes=%d", stored, first.closeCalls.Load())
	}
}

func TestLaunchPasswordCaptchaReplaceFailureLeavesButtonUsable(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	first := newTestCaptchaBrowserController()
	replacement := newTestCaptchaBrowserController()
	var launches atomic.Int32
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		switch launches.Add(1) {
		case 1:
			return first, nil
		case 2:
			return nil, errors.New("temporary chrome failure")
		default:
			return replacement, nil
		}
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err == nil {
		t.Fatal("replacement launch unexpectedly succeeded")
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatalf("retry after replacement failure: %v", err)
	}
	if first.closeCalls.Load() != 1 || launches.Load() != 3 {
		t.Fatalf("first closes=%d launches=%d", first.closeCalls.Load(), launches.Load())
	}
}

func TestLaunchPasswordCaptchaQueuedDuringTerminalOutcomeIsRejected(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	closeRelease := make(chan struct{})
	first := newTestCaptchaBrowserController()
	first.closeRelease = closeRelease
	second := newTestCaptchaBrowserController()
	secondLaunchCalled := make(chan struct{}, 1)
	var launches atomic.Int32
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		if launches.Add(1) == 1 {
			return first, nil
		}
		secondLaunchCalled <- struct{}{}
		return second, nil
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}

	outcomeDone := make(chan struct{})
	go func() {
		_, _ = s.setPasswordOutcome(state, flow, passwordOutcome{display: "Player#KR1"})
		close(outcomeDone)
	}()
	<-first.closeStarted
	launchDone := make(chan error, 1)
	go func() {
		launchDone <- s.LaunchPasswordCaptcha(context.Background(), state, "owner-1")
	}()

	var queuedErr error
	launchedWhileFinishing := false
	select {
	case queuedErr = <-launchDone:
	case <-secondLaunchCalled:
		launchedWhileFinishing = true
	}
	close(closeRelease)
	<-outcomeDone
	if launchedWhileFinishing {
		queuedErr = <-launchDone
	}
	if queuedErr == nil {
		t.Fatal("queued launch succeeded while terminal outcome was publishing")
	}
	if launches.Load() != 1 {
		t.Fatalf("launcher calls=%d, want no call after terminal sealing", launches.Load())
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err == nil {
		t.Fatal("repeated launch succeeded after terminal outcome was published")
	}
	if launches.Load() != 1 {
		t.Fatalf("launcher calls=%d after repeated click, want 1", launches.Load())
	}
}

func TestLaunchPasswordCaptchaCloseFailureRetainsOwnedBrowser(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	closeFailure := errors.New("owned Chrome process may still be running")
	first := newTestCaptchaBrowserController()
	first.closeErr = closeFailure
	replacement := newTestCaptchaBrowserController()
	var launches atomic.Int32
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		if launches.Add(1) == 1 {
			return first, nil
		}
		return replacement, nil
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); !errors.Is(err, closeFailure) {
		t.Fatalf("reopen error=%v, want close failure", err)
	}
	if launches.Load() != 1 {
		t.Fatalf("launcher calls=%d, replacement must not launch", launches.Load())
	}
	flow.launchMu.Lock()
	owned := flow.browser
	flow.launchMu.Unlock()
	if owned != first {
		t.Fatalf("retained controller=%T, want original controller", owned)
	}
}

func TestCaptchaBrowserCloseFailureConvertsPublishedOutcomeToError(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  passwordOutcome
	}{
		{name: "success", out: passwordOutcome{display: "Player#KR1"}},
		{name: "mfa", out: passwordOutcome{mfaState: "mfa-state", mfaHint: "email"}},
		{name: "terminal error", out: passwordOutcome{err: errors.New("riot rejected captcha")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCaptchaServer(&fakePasswordAuth{})
			closeFailure := errors.New("owned Chrome process may still be running")
			controller := newTestCaptchaBrowserController()
			controller.closeErr = closeFailure
			s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
			_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			flow := s.passwordPending[state].flow
			s.mu.Unlock()
			if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
				t.Fatal(err)
			}
			_, _ = s.setPasswordOutcome(state, flow, tc.out)
			s.mu.Lock()
			published := s.passwordOutcomes[state]
			s.mu.Unlock()
			if !published.done || !errors.Is(published.err, closeFailure) {
				t.Fatalf("published outcome=%+v, want terminal browser-close error", published)
			}
			if published.display != "" || published.mfaState != "" || published.mfaHint != "" {
				t.Fatalf("close failure exposed success/MFA outcome: %+v", published)
			}
			flow.launchMu.Lock()
			owned := flow.browser
			flow.launchMu.Unlock()
			if owned != controller {
				t.Fatalf("retained controller=%T, want failed controller", owned)
			}
		})
	}
}

func TestCaptchaBrowserCloseFailureRemovesOrphanMFA(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:  riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa: &riot.MFAChallenge{Email: "a***@example.com", Method: "email"},
	}
	s := newCaptchaServer(pw)
	closeFailure := errors.New("owned Chrome process may still be running")
	controller := newTestCaptchaBrowserController()
	controller.closeErr = closeFailure
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}
	req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var response map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["ok"] != false || !strings.Contains(fmt.Sprint(response["error"]), closeFailure.Error()) {
		t.Fatalf("submit response=%+v, want browser-close failure", response)
	}
	s.mu.Lock()
	mfaCount := len(s.mfaPending)
	s.mu.Unlock()
	if mfaCount != 0 {
		t.Fatalf("orphan MFA states=%d, want 0", mfaCount)
	}
	_, mfaState, _, waitErr := s.WaitPasswordLogin(context.Background(), state)
	if !errors.Is(waitErr, closeFailure) || mfaState != "" {
		t.Fatalf("wait error=%v mfaState=%q", waitErr, mfaState)
	}
}

func TestCaptchaSubmitCleanupRaceRejectsMFAAndRemovesContinuation(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:  riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa: &riot.MFAChallenge{Email: "a***@example.com", Method: "email"},
	}
	s := newCaptchaServer(pw)
	closeFailure := errors.New("owned Chrome process may still be running")
	controller := newTestCaptchaBrowserController()
	controller.closeErr = closeFailure
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()

	closeRelease := make(chan struct{})
	controller.closeRelease = closeRelease
	rec := httptest.NewRecorder()
	submitDone := make(chan struct{})
	go func() {
		defer close(submitDone)
		req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
		req.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rec, req)
	}()
	<-controller.closeStarted
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		s.cleanupPasswordState(state)
	}()
	close(closeRelease)
	<-submitDone
	<-cleanupDone

	var response map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["ok"] != false || response["mfa"] == true {
		t.Fatalf("cleanup race response=%+v, want terminal closure", response)
	}
	s.mu.Lock()
	_, passwordExists := s.passwordPending[state]
	mfaCount := len(s.mfaPending)
	failure, retained := s.captchaCloseFailures[flow]
	s.mu.Unlock()
	if passwordExists || mfaCount != 0 {
		t.Fatalf("password exists=%v orphan MFA states=%d", passwordExists, mfaCount)
	}
	if !retained || failure.controller != controller || !errors.Is(failure.err, closeFailure) || !failure.possiblyRunning {
		t.Fatalf("retained close failure=%+v exists=%v", failure, retained)
	}
}

func TestCaptchaSubmitCleanupRaceRejectsAccountLink(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:     riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		tokens: riot.PasswordTokens{AccessToken: "at-1", IDToken: "id-1", SessionCookie: "ssid=s1"},
	}
	s := newCaptchaServer(pw)
	upsertRelease := make(chan struct{})
	st := &blockingCaptchaStore{
		mockStore:     newMockStore(),
		upsertStarted: make(chan struct{}, 1),
		upsertRelease: upsertRelease,
	}
	s.store = st
	closeFailure := errors.New("owned Chrome process may still be running")
	closeRelease := make(chan struct{})
	controller := newTestCaptchaBrowserController()
	controller.closeErr = closeFailure
	controller.closeRelease = closeRelease
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()

	rec := httptest.NewRecorder()
	submitDone := make(chan struct{})
	go func() {
		defer close(submitDone)
		req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
		req.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rec, req)
	}()

	// The old ordering reaches account persistence first. The corrected
	// ordering reaches browser finalization first. Either signal is a
	// deterministic point after the last liveness check and before publish.
	select {
	case <-st.upsertStarted:
		cleanupDone := make(chan struct{})
		go func() {
			defer close(cleanupDone)
			s.cleanupPasswordState(state)
		}()
		deadline := time.Now().Add(time.Second)
		for {
			s.mu.Lock()
			_, exists := s.passwordPending[state]
			s.mu.Unlock()
			if !exists {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("cleanup did not remove password state")
			}
			runtime.Gosched()
		}
		<-controller.closeStarted
		close(closeRelease)
		close(upsertRelease)
		<-submitDone
		<-cleanupDone
	case <-controller.closeStarted:
		cleanupDone := make(chan struct{})
		go func() {
			defer close(cleanupDone)
			s.cleanupPasswordState(state)
		}()
		close(closeRelease)
		close(upsertRelease)
		<-submitDone
		<-cleanupDone
	case <-time.After(time.Second):
		t.Fatal("submission did not reach its pre-publication window")
	}

	var response map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["ok"] != false {
		t.Fatalf("cleanup race response=%+v, want terminal closure", response)
	}
	if len(st.accounts) != 0 {
		t.Fatalf("cleanup race linked accounts=%+v, want none", st.accounts)
	}
	s.mu.Lock()
	failure, retained := s.captchaCloseFailures[flow]
	s.mu.Unlock()
	if !retained || failure.controller != controller || !errors.Is(failure.err, closeFailure) || !failure.possiblyRunning {
		t.Fatalf("retained close failure=%+v exists=%v", failure, retained)
	}
}

func TestSetPasswordOutcomeRejectsCleanedState(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	closeFailure := errors.New("owned Chrome process may still be running")
	controller := newTestCaptchaBrowserController()
	controller.closeErr = closeFailure
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.cleanupPasswordState(state)

	published, publishErr := s.setPasswordOutcome(state, flow, passwordOutcome{display: "Player#KR1"})
	if !errors.Is(publishErr, errPasswordFlowClosed) {
		t.Fatalf("publish error=%v, want terminal flow closure", publishErr)
	}
	if published.done || published.display != "" {
		t.Fatalf("cleaned state published outcome: %+v", published)
	}
	s.mu.Lock()
	_, pending := s.passwordPending[state]
	_, outcome := s.passwordOutcomes[state]
	failure, retained := s.captchaCloseFailures[flow]
	s.mu.Unlock()
	if pending || outcome {
		t.Fatalf("cleaned state revived: pending=%v outcome=%v", pending, outcome)
	}
	if !retained || failure.controller != controller || !errors.Is(failure.err, closeFailure) || !failure.possiblyRunning {
		t.Fatalf("retained close failure=%+v exists=%v", failure, retained)
	}
}

func TestCaptchaSubmitCancellationWinsDuringAccountPreparation(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:     riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		tokens: riot.PasswordTokens{AccessToken: "at-1", IDToken: "id-1", SessionCookie: "ssid=s1"},
	}
	s := newCaptchaServer(pw)
	namesRelease := make(chan struct{})
	s.riot = &cancelBlockingRiot{
		mockRiot: &mockRiot{
			entitlements: "ent",
			puuid:        "puuid-1",
			names:        []riot.PlayerName{{GameName: "Player", TagLine: "KR1"}},
			region:       "kr",
			shard:        "kr",
		},
		namesStarted: make(chan struct{}, 1),
		namesRelease: namesRelease,
	}
	st := s.store.(*mockStore)
	var linked atomic.Int32
	s.onLinked = func(string, string) { linked.Add(1) }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	submitDone := make(chan struct{})
	go func() {
		defer close(submitDone)
		req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
		req.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rec, req)
	}()
	r := s.riot.(*cancelBlockingRiot)
	<-r.namesStarted

	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		s.cleanupPasswordState(state)
	}()
	canceledPromptly := false
	select {
	case <-flow.ctx.Done():
		canceledPromptly = true
	case <-time.After(150 * time.Millisecond):
	}
	close(namesRelease)
	<-submitDone
	<-cleanupDone

	var response map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !canceledPromptly {
		t.Error("cleanup did not signal flow cancellation while Riot identity preparation was blocked")
	}
	if response["ok"] != false {
		t.Errorf("canceled preparation response=%+v, want terminal closure", response)
	}
	if len(st.accounts) != 0 || linked.Load() != 0 {
		t.Errorf("canceled preparation accounts=%+v onLinked=%d, want no irreversible side effects", st.accounts, linked.Load())
	}
}

func TestCaptchaSubmitCancellationWinsBeforeMFAPublish(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:  riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa: &riot.MFAChallenge{Email: "a***@example.com", Method: "email"},
	}
	s := newCaptchaServer(pw)
	closeRelease := make(chan struct{})
	controller := newTestCaptchaBrowserController()
	controller.closeRelease = closeRelease
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	submitDone := make(chan struct{})
	go func() {
		defer close(submitDone)
		req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
		req.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rec, req)
	}()
	<-controller.closeStarted
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		s.cleanupPasswordState(state)
	}()
	canceledPromptly := false
	select {
	case <-flow.ctx.Done():
		canceledPromptly = true
	case <-time.After(150 * time.Millisecond):
	}
	close(closeRelease)
	<-submitDone
	<-cleanupDone

	var response map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !canceledPromptly {
		t.Error("cleanup did not signal flow cancellation while successful Chrome close was blocked")
	}
	if response["ok"] != false || response["mfa"] == true {
		t.Errorf("canceled MFA response=%+v, want terminal closure", response)
	}
	s.mu.Lock()
	mfaCount := len(s.mfaPending)
	s.mu.Unlock()
	if mfaCount != 0 {
		t.Errorf("canceled MFA left %d continuation(s)", mfaCount)
	}
}

func TestCaptchaSubmitAccountCommitClaimWinsBeforeCleanup(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:     riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		tokens: riot.PasswordTokens{AccessToken: "at-1", IDToken: "id-1", SessionCookie: "ssid=s1"},
	}
	s := newCaptchaServer(pw)
	upsertRelease := make(chan struct{})
	st := &blockingCaptchaStore{
		mockStore:     newMockStore(),
		upsertStarted: make(chan struct{}, 1),
		upsertRelease: upsertRelease,
	}
	s.store = st
	var linked atomic.Int32
	s.onLinked = func(string, string) { linked.Add(1) }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	submitDone := make(chan struct{})
	go func() {
		defer close(submitDone)
		req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
		req.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rec, req)
	}()
	<-st.upsertStarted
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		s.cleanupPasswordState(state)
	}()
	cleanupObservedCommit := false
	select {
	case <-cleanupDone:
		cleanupObservedCommit = true
	case <-time.After(150 * time.Millisecond):
	}
	flowCanceledBeforeCommit := false
	select {
	case <-flow.ctx.Done():
		flowCanceledBeforeCommit = true
	default:
	}
	close(upsertRelease)
	<-submitDone
	<-cleanupDone

	var response map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !cleanupObservedCommit {
		t.Error("cleanup blocked behind an already-claimed account commit")
	}
	if flowCanceledBeforeCommit {
		t.Error("cleanup canceled a flow after its irreversible commit claim won")
	}
	if response["ok"] != true || len(st.accounts) != 1 || linked.Load() != 1 {
		t.Errorf("commit-wins response=%+v accounts=%+v onLinked=%d", response, st.accounts, linked.Load())
	}
	select {
	case <-flow.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("automatic cleanup did not run after the requested commit finished")
	}
	s.mu.Lock()
	_, pending := s.passwordPending[state]
	_, outcome := s.passwordOutcomes[state]
	_, ready := s.passwordReady[state]
	s.mu.Unlock()
	if pending || outcome || ready {
		t.Fatalf("automatic cleanup retained pending=%v outcome=%v ready=%v", pending, outcome, ready)
	}
}

func TestCaptchaBrowserCloseFailurePreventsSuccessOrTerminalResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		complete error
		tokens   riot.PasswordTokens
	}{
		{
			name:   "success",
			tokens: riot.PasswordTokens{AccessToken: "at-1", IDToken: "id-1", SessionCookie: "ssid=s1"},
		},
		{
			name:     "terminal error",
			complete: errors.New("riot rejected captcha"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pw := &fakePasswordAuth{
				ch:       riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
				tokens:   tc.tokens,
				complete: tc.complete,
			}
			s := newCaptchaServer(pw)
			closeFailure := errors.New("owned Chrome process may still be running")
			controller := newTestCaptchaBrowserController()
			controller.closeErr = closeFailure
			s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
			_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
				t.Fatal(err)
			}
			req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			var response map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response["ok"] != false || !strings.Contains(fmt.Sprint(response["error"]), closeFailure.Error()) {
				t.Fatalf("submit response=%+v, want browser-close failure", response)
			}
			display, mfaState, _, waitErr := s.WaitPasswordLogin(context.Background(), state)
			if !errors.Is(waitErr, closeFailure) || display != "" || mfaState != "" {
				t.Fatalf("display=%q mfaState=%q wait error=%v", display, mfaState, waitErr)
			}
		})
	}
}

func TestCaptchaBrowserCleanupFailureRetainsOwnership(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	closeFailure := errors.New("owned Chrome process may still be running")
	controller := newTestCaptchaBrowserController()
	controller.closeErr = closeFailure
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.cleanupPasswordState(state)
	flow.launchMu.Lock()
	owned := flow.browser
	flow.launchMu.Unlock()
	if owned != controller {
		t.Fatalf("cleanup discarded failed controller %T", owned)
	}
	s.mu.Lock()
	failure, recorded := s.captchaCloseFailures[flow]
	s.mu.Unlock()
	if !recorded || failure.controller != controller || !errors.Is(failure.err, closeFailure) || !failure.possiblyRunning {
		t.Fatalf("recorded close failure=%+v exists=%v", failure, recorded)
	}
}

func TestCaptchaBrowserExitedProfileFailureDoesNotReplaceSuccess(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	profileFailure := errors.New("state profile could not be removed")
	controller := newTestCaptchaBrowserController()
	controller.closeErr = &captchaBrowserCloseError{ProcessExited: true, Err: profileFailure}
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.setPasswordOutcome(state, flow, passwordOutcome{display: "Player#KR1"})
	s.mu.Lock()
	published := s.passwordOutcomes[state]
	failure, recorded := s.captchaCloseFailures[flow]
	s.mu.Unlock()
	if published.err != nil || published.display != "Player#KR1" {
		t.Fatalf("profile-only cleanup error replaced success: %+v", published)
	}
	if !recorded || failure.controller != controller || !errors.Is(failure.err, profileFailure) || failure.possiblyRunning {
		t.Fatalf("recorded profile failure=%+v exists=%v", failure, recorded)
	}
}

func TestCaptchaBrowserLaunchedAfterExpiryClosesInsteadOfInstalling(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	controller := newTestCaptchaBrowserController()
	launchStarted := make(chan struct{})
	releaseLaunch := make(chan struct{})
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		close(launchStarted)
		<-releaseLaunch
		return controller, nil
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	launchDone := make(chan error, 1)
	go func() {
		launchDone <- s.LaunchPasswordCaptcha(context.Background(), state, "owner-1")
	}()
	<-launchStarted
	s.mu.Lock()
	pending := s.passwordPending[state]
	pending.expiresAt = time.Now().Add(-time.Second)
	s.passwordPending[state] = pending
	s.mu.Unlock()
	close(releaseLaunch)
	if err := <-launchDone; err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("launch error=%v, want expiry", err)
	}
	if controller.closeCalls.Load() != 1 {
		t.Fatalf("late controller closes=%d, want 1", controller.closeCalls.Load())
	}
	flow.launchMu.Lock()
	stored := flow.browser
	flow.launchMu.Unlock()
	if stored != nil {
		t.Fatalf("expired state retained controller %T", stored)
	}
}

func TestCaptchaBrowserLaunchedAfterExpiryRetainsCloseFailure(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	closeFailure := errors.New("late Chrome process may still be running")
	controller := newTestCaptchaBrowserController()
	controller.closeErr = closeFailure
	launchStarted := make(chan struct{})
	releaseLaunch := make(chan struct{})
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		close(launchStarted)
		<-releaseLaunch
		return controller, nil
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	launchDone := make(chan error, 1)
	go func() {
		launchDone <- s.LaunchPasswordCaptcha(context.Background(), state, "owner-1")
	}()
	<-launchStarted
	s.mu.Lock()
	pending := s.passwordPending[state]
	pending.expiresAt = time.Now().Add(-time.Second)
	s.passwordPending[state] = pending
	s.mu.Unlock()
	close(releaseLaunch)
	if err := <-launchDone; !errors.Is(err, closeFailure) {
		t.Fatalf("launch error=%v, want actionable cleanup failure", err)
	}
	flow.launchMu.Lock()
	owned := flow.browser
	flow.launchMu.Unlock()
	if owned != controller {
		t.Fatalf("late failed controller=%T, want retained ownership", owned)
	}
	s.mu.Lock()
	failure, recorded := s.captchaCloseFailures[flow]
	s.mu.Unlock()
	if !recorded || failure.controller != controller || !errors.Is(failure.err, closeFailure) || !failure.possiblyRunning {
		t.Fatalf("recorded late close failure=%+v exists=%v", failure, recorded)
	}
}

func TestCaptchaBrowserClosesBeforePasswordWaitReturns(t *testing.T) {
	for _, tc := range []struct {
		name string
		mfa  *riot.MFAChallenge
	}{
		{name: "completion"},
		{name: "mfa", mfa: &riot.MFAChallenge{Email: "a***@example.com", Method: "email"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pw := &fakePasswordAuth{
				ch:     riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
				tokens: riot.PasswordTokens{AccessToken: "at-1", IDToken: "id-1", SessionCookie: "ssid=s1"},
				mfa:    tc.mfa,
			}
			s := newCaptchaServer(pw)
			releaseClose := make(chan struct{})
			controller := newTestCaptchaBrowserController()
			controller.closeRelease = releaseClose
			s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
			_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
				t.Fatal(err)
			}

			waitDone := make(chan error, 1)
			go func() {
				_, _, _, waitErr := s.WaitPasswordLogin(context.Background(), state)
				waitDone <- waitErr
			}()
			submitDone := make(chan struct{})
			go func() {
				defer close(submitDone)
				req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
				req.Header.Set("Content-Type", "application/json")
				s.Handler().ServeHTTP(httptest.NewRecorder(), req)
			}()

			<-controller.closeStarted
			select {
			case err := <-waitDone:
				t.Fatalf("WaitPasswordLogin returned before browser close completed: %v", err)
			default:
			}
			close(releaseClose)
			<-submitDone
			if err := <-waitDone; err != nil {
				t.Fatal(err)
			}
			if controller.closeCalls.Load() != 1 {
				t.Fatalf("browser closes=%d, want 1", controller.closeCalls.Load())
			}
		})
	}
}

func TestCaptchaBrowserClosesOnTerminalErrorCancellationAndExpiry(t *testing.T) {
	t.Run("terminal error", func(t *testing.T) {
		pw := &fakePasswordAuth{
			ch:       riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
			complete: errors.New("terminal captcha failure"),
		}
		s := newCaptchaServer(pw)
		controller := newTestCaptchaBrowserController()
		s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
		_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
			t.Fatal(err)
		}
		req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
		req.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
		_, _, _, waitErr := s.WaitPasswordLogin(context.Background(), state)
		if waitErr == nil || controller.closeCalls.Load() != 1 {
			t.Fatalf("wait error=%v browser closes=%d", waitErr, controller.closeCalls.Load())
		}
	})

	t.Run("wait cancellation", func(t *testing.T) {
		s := newCaptchaServer(&fakePasswordAuth{})
		controller := newTestCaptchaBrowserController()
		s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
		_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, _, waitErr := s.WaitPasswordLogin(ctx, state)
		if !errors.Is(waitErr, context.Canceled) || controller.closeCalls.Load() != 1 {
			t.Fatalf("wait error=%v browser closes=%d", waitErr, controller.closeCalls.Load())
		}
	})

	t.Run("expiry", func(t *testing.T) {
		s := newCaptchaServer(&fakePasswordAuth{})
		controller := newTestCaptchaBrowserController()
		s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
		_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		pending := s.passwordPending[state]
		pending.expiresAt = time.Now().Add(-time.Second)
		s.passwordPending[state] = pending
		s.mu.Unlock()
		s.expirePasswordState(state)
		if controller.closeCalls.Load() != 1 {
			t.Fatalf("browser closes=%d, want 1", controller.closeCalls.Load())
		}
	})
}

func TestPasswordOutcomeCleanupErasesCredentials(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "riot-user", "secret-password")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()
	_, _ = s.setPasswordOutcome(state, flow, passwordOutcome{err: errors.New("terminal")})
	s.mu.Lock()
	pending, ok := s.passwordPending[state]
	s.mu.Unlock()
	if !ok {
		t.Fatal("pending state removed before waiter could consume outcome")
	}
	if pending.username != "" || pending.password != "" {
		t.Fatalf("credentials retained after terminal outcome: username=%q password length=%d", pending.username, len(pending.password))
	}
}

func TestWaitPasswordLoginCancellationClearsCredentialsAndRiotSession(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{SessionID: "sess-to-cancel", SiteKey: "site-key", RQData: "rq-data"},
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "secret-password")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := s.ensureCaptchaChallenge(ctx, state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}
	_, _, _, waitErr := s.WaitPasswordLogin(ctx, state)
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v", waitErr)
	}
	s.mu.Lock()
	_, pending := s.passwordPending[state]
	_, outcome := s.passwordOutcomes[state]
	s.mu.Unlock()
	if pending || outcome {
		t.Fatalf("timed-out state retained: pending=%v outcome=%v", pending, outcome)
	}
	if got, _ := pw.canceledSession.Load().(string); got != "sess-to-cancel" {
		t.Fatalf("canceled Riot session = %q", got)
	}
}

func TestWaitPasswordLoginOutcomeWinsConcurrentCancellationAndKeepsMFAUsable(t *testing.T) {
	pw := &fakePasswordAuth{
		tokens: riot.PasswordTokens{AccessToken: "at-1", IDToken: "id-1", SessionCookie: "ssid=s1"},
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "secret-password")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	pending := s.passwordPending[state]
	s.mu.Unlock()

	const wantMFAState = "mfa-outcome-won"
	challenge := &riot.MFAChallenge{Email: "a***@example.com", Method: "email"}
	var published passwordOutcome
	var publishErr error
	ctx := newCallbackCanceledContext(func() {
		published, publishErr = s.finishPasswordMFA(state, pending, wantMFAState, challenge, challenge.Email)
	})

	display, mfaState, mfaHint, waitErr := s.WaitPasswordLogin(ctx, state)
	if publishErr != nil {
		t.Fatalf("MFA outcome publication error = %v", publishErr)
	}
	if !published.done || published.mfaState != wantMFAState {
		t.Fatalf("published outcome = %+v", published)
	}
	if waitErr != nil || display != "" || mfaState != wantMFAState || mfaHint != challenge.Email {
		t.Fatalf("wait display=%q mfaState=%q hint=%q err=%v", display, mfaState, mfaHint, waitErr)
	}

	s.mu.Lock()
	_, passwordPending := s.passwordPending[state]
	_, passwordOutcome := s.passwordOutcomes[state]
	continuation, continuationExists := s.mfaPending[wantMFAState]
	mfaCount := len(s.mfaPending)
	s.mu.Unlock()
	if passwordPending || passwordOutcome {
		t.Fatalf("consumed password state retained: pending=%v outcome=%v", passwordPending, passwordOutcome)
	}
	if !continuationExists || mfaCount != 1 || continuation.flow == nil || continuation.flow.ctx.Err() != nil {
		t.Fatalf("MFA continuation exists=%v count=%d value=%+v", continuationExists, mfaCount, continuation)
	}

	linkedDisplay, err := s.CompletePasswordMFA(context.Background(), wantMFAState, "owner-1", "123456")
	if err != nil || linkedDisplay != "Player#KR1" {
		t.Fatalf("usable MFA continuation display=%q err=%v", linkedDisplay, err)
	}
	if accounts := s.store.(*mockStore).accounts; len(accounts) != 1 {
		t.Fatalf("MFA continuation linked accounts=%+v", accounts)
	}
}

func TestWaitPasswordLoginCancellationWinsBeforeLaterPublishers(t *testing.T) {
	pw := &fakePasswordAuth{
		tokens: riot.PasswordTokens{AccessToken: "at-1", IDToken: "id-1", SessionCookie: "ssid=s1"},
	}
	s := newCaptchaServer(pw)
	releaseClose := make(chan struct{})
	var releaseCloseOnce sync.Once
	releaseBrowser := func() { releaseCloseOnce.Do(func() { close(releaseClose) }) }
	defer releaseBrowser()
	controller := newTestCaptchaBrowserController()
	controller.closeRelease = releaseClose
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) { return controller, nil }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "secret-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	pending := s.passwordPending[state]
	s.mu.Unlock()
	claimReached := make(chan struct{})
	allowClaim := make(chan struct{})
	var allowClaimOnce sync.Once
	releaseClaim := func() { allowClaimOnce.Do(func() { close(allowClaim) }) }
	defer releaseClaim()
	s.beforePasswordWaitCancellationClaim = func() {
		close(claimReached)
		<-allowClaim
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitDone := make(chan error, 1)
	go func() {
		_, _, _, waitErr := s.WaitPasswordLogin(ctx, state)
		waitDone <- waitErr
	}()
	select {
	case <-claimReached:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not reach the cancellation claim")
	}
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("cancellation outcome check and state detach do not share Server.mu")
	}

	type publishErrors struct {
		mfa     error
		account error
	}
	publishDone := make(chan publishErrors, 1)
	go func() {
		_, mfaErr := s.finishPasswordMFA(state, pending, "late-mfa", &riot.MFAChallenge{Email: "late@example.com"}, "late@example.com")
		_, accountErr := s.finishPasswordAccount(context.Background(), state, pending, pw.tokens)
		publishDone <- publishErrors{mfa: mfaErr, account: accountErr}
	}()
	releaseClaim()
	select {
	case <-controller.closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("controller close did not start after cancellation claimed state")
	}

	s.mu.Lock()
	_, passwordPending := s.passwordPending[state]
	_, passwordOutcome := s.passwordOutcomes[state]
	s.mu.Unlock()
	if passwordPending || passwordOutcome {
		t.Fatalf("cancellation claim retained state while controller close blocked: pending=%v outcome=%v", passwordPending, passwordOutcome)
	}
	select {
	case <-pending.flow.ctx.Done():
	default:
		t.Fatal("cancellation claim did not cancel the flow before controller close")
	}

	var publisherErrors publishErrors
	select {
	case publisherErrors = <-publishDone:
	case <-time.After(2 * time.Second):
		t.Fatal("late publishers remained blocked while controller close ran outside Server.mu")
	}
	if !errors.Is(publisherErrors.mfa, errPasswordFlowClosed) {
		t.Fatalf("late MFA publisher error = %v, want closed flow", publisherErrors.mfa)
	}
	if !errors.Is(publisherErrors.account, errPasswordFlowClosed) {
		t.Fatalf("late account publisher error = %v, want closed flow", publisherErrors.account)
	}
	s.mu.Lock()
	mfaCount := len(s.mfaPending)
	s.mu.Unlock()
	if mfaCount != 0 || len(s.store.(*mockStore).accounts) != 0 {
		t.Fatalf("late publishers produced mfa=%d accounts=%+v", mfaCount, s.store.(*mockStore).accounts)
	}

	releaseBrowser()
	select {
	case waitErr := <-waitDone:
		if !errors.Is(waitErr, context.Canceled) {
			t.Fatalf("wait error = %v, want context cancellation", waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return after controller close")
	}
	if controller.closeCalls.Load() != 1 {
		t.Fatalf("browser closes=%d, want 1", controller.closeCalls.Load())
	}
}

func TestWaitPasswordLoginCancellationDoesNotYieldToContendingMFAPublisher(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	const state = "cancellation-publisher-race"
	flowCtx, flowCancel := context.WithCancel(context.Background())
	defer flowCancel()
	pending := passwordPending{
		discordUserID: "owner-1",
		username:      "user",
		password:      "secret-password",
		flow: &passwordFlow{
			ctx:    flowCtx,
			cancel: flowCancel,
		},
		expiresAt: time.Now().Add(time.Minute),
	}
	s.mu.Lock()
	s.passwordPending[state] = pending
	s.passwordOutcomes[state] = passwordOutcome{}
	s.passwordReady[state] = make(chan struct{})
	s.mu.Unlock()

	publisherAtLock := make(chan struct{})
	allowPublisherAttempt := make(chan struct{})
	publisherAttempted := make(chan bool)
	publisherDone := make(chan struct{})
	syncFailures := make(chan error, 4)
	recordSyncFailure := func(err error) {
		select {
		case syncFailures <- err:
		default:
		}
	}
	var releasePublisherOnce sync.Once
	releasePublisher := func() { releasePublisherOnce.Do(func() { close(allowPublisherAttempt) }) }
	defer releasePublisher()

	var lockEntry atomic.Int32
	s.mu.lockForTest = func(mu *sync.Mutex) {
		entry := lockEntry.Add(1)
		switch entry {
		case 1:
			// The real MFA publisher's finalization claim must complete before it
			// can reach the outcome-publication lock covered by this regression.
			mu.Lock()
		case 2:
			close(publisherAtLock)
			select {
			case <-allowPublisherAttempt:
			case <-time.After(2 * time.Second):
				recordSyncFailure(errors.New("publisher was not released to attempt Server.mu"))
			}
			acquired := mu.TryLock()
			select {
			case publisherAttempted <- acquired:
			case <-time.After(2 * time.Second):
				recordSyncFailure(errors.New("publisher lock-attempt handshake timed out"))
			}
			if !acquired {
				mu.Lock()
			}
		default:
			if entry >= 5 {
				// A split-lock cancellation claimant enters here before trying to
				// barge ahead of the queued publisher. Hold that relock until the
				// real publisher has completed its publication attempt.
				select {
				case <-publisherDone:
				case <-time.After(2 * time.Second):
					recordSyncFailure(errors.New("later Server.mu entrant timed out waiting for publisher"))
				}
			}
			mu.Lock()
		}
	}

	type publishResult struct {
		out passwordOutcome
		err error
	}
	published := make(chan publishResult, 1)
	go func() {
		out, publishErr := s.finishPasswordMFA(
			state,
			pending,
			"contending-mfa",
			&riot.MFAChallenge{Email: "late@example.com", Method: "email"},
			"late@example.com",
		)
		published <- publishResult{out: out, err: publishErr}
		close(publisherDone)
	}()
	select {
	case <-publisherAtLock:
	case <-time.After(2 * time.Second):
		t.Fatal("MFA publisher did not reach its outcome-publication Server.mu boundary")
	}

	claimReached := make(chan struct{})
	publisherContending := make(chan struct{})
	allowClaim := make(chan struct{})
	var publisherAcquired atomic.Bool
	var releaseClaimOnce sync.Once
	releaseClaim := func() { releaseClaimOnce.Do(func() { close(allowClaim) }) }
	defer releaseClaim()
	s.beforePasswordWaitCancellationClaim = func() {
		close(claimReached)
		releasePublisher()
		select {
		case acquired := <-publisherAttempted:
			publisherAcquired.Store(acquired)
		case <-time.After(2 * time.Second):
			recordSyncFailure(errors.New("cancellation claimant did not observe publisher lock attempt"))
		}
		close(publisherContending)
		select {
		case <-allowClaim:
		case <-time.After(2 * time.Second):
			recordSyncFailure(errors.New("cancellation claimant release handshake timed out"))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitDone := make(chan error, 1)
	go func() {
		_, _, _, waitErr := s.WaitPasswordLogin(ctx, state)
		waitDone <- waitErr
	}()
	select {
	case <-claimReached:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not reach the cancellation claim")
	}
	select {
	case <-publisherContending:
	case <-time.After(2 * time.Second):
		t.Fatal("MFA publisher did not contend on Server.mu")
	}
	if publisherAcquired.Load() {
		t.Error("MFA publisher unexpectedly acquired Server.mu while cancellation claimant held it")
	}
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("cancellation claimant released Server.mu before state detach")
	}
	releaseClaim()

	var publish publishResult
	select {
	case publish = <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("MFA publisher did not finish after cancellation claim")
	}
	select {
	case waitErr := <-waitDone:
		if !errors.Is(waitErr, context.Canceled) {
			t.Fatalf("wait error = %v, want context cancellation", waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return after cancellation claim")
	}
	if !errors.Is(publish.err, errPasswordFlowClosed) {
		t.Errorf("contending MFA publisher error = %v, want closed flow", publish.err)
	}
	if publish.out.done {
		t.Errorf("contending MFA publisher published %+v after cancellation won", publish.out)
	}

	s.mu.Lock()
	_, passwordPending := s.passwordPending[state]
	_, passwordOutcome := s.passwordOutcomes[state]
	_, mfaContinuation := s.mfaPending["contending-mfa"]
	s.mu.Unlock()
	if passwordPending || passwordOutcome || mfaContinuation {
		t.Errorf(
			"cancellation cleanup left pending=%v outcome=%v mfa=%v",
			passwordPending,
			passwordOutcome,
			mfaContinuation,
		)
	}
	select {
	case syncErr := <-syncFailures:
		t.Fatal(syncErr)
	default:
	}
}

func TestLaunchPasswordCaptchaReturnsPreparationFailure(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
	}
	s := newCaptchaServer(pw)
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		return nil, errors.New("Chrome/Chromium not found")
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	err = s.LaunchPasswordCaptcha(context.Background(), state, "owner-1")
	if err == nil || !strings.Contains(err.Error(), "Chrome/Chromium") {
		t.Fatalf("launch error = %v", err)
	}
}

func TestCaptchaWidgetPage_ExecutesInvisibleChallengeWithRQData(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{
		ch: riot.CaptchaChallenge{SessionID: "s", SiteKey: "k", RQData: "d"},
	})
	_, state, err := s.BeginPasswordLogin(context.Background(), "discord-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/captcha/widget?state="+state, nil)
	req.Host = RiotCaptchaHost
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/api/auth/captcha/challenge") {
		t.Fatalf("widget should fetch challenge")
	}
	if !strings.Contains(body, "size: 'invisible'") {
		t.Fatal("widget must match Riot's invisible hCaptcha mode")
	}
	if !strings.Contains(body, "hcaptcha.execute(widgetId, {rqdata: rqdata})") {
		t.Fatal("widget must attach the current rqdata when executing hCaptcha")
	}
	if strings.Contains(body, "hcaptcha.setData") || strings.Contains(body, "size: 'normal'") {
		t.Fatal("widget must not use the rejected checkbox/setData flow")
	}
	if !strings.Contains(body, `id="verify"`) {
		t.Fatal("invisible captcha needs an explicit user verification control")
	}
	if !strings.Contains(body, "async function refreshCaptchaChallenge") ||
		!strings.Contains(body, "await refreshCaptchaChallenge()") ||
		!strings.Contains(body, "if (data && data.reload)") ||
		!strings.Contains(body, "renderWidget(false)") {
		t.Fatal("a lost submit response must reload the current challenge instead of reusing its token")
	}
	if strings.Contains(body, "host !== 'auth.riotgames.com'") {
		t.Fatal("widget must reject the legacy Riot OAuth hostname")
	}
}

func TestCaptchaWidgetPageConcurrentSealingIsRaceSafe(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	flow := s.passwordPending[state].flow
	s.mu.Unlock()

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 2000 {
				req := newCaptchaRequest(http.MethodGet, "/captcha/widget?state="+state, nil)
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
					t.Errorf("widget status=%d", rec.Code)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := range 8000 {
			s.mu.Lock()
			flow.sealed = i%2 == 0
			s.mu.Unlock()
			runtime.Gosched()
		}
	}()
	close(start)
	wg.Wait()

	s.mu.Lock()
	flow.sealed = false
	s.mu.Unlock()
}

func TestCaptchaChallengeAndSubmit_NoMFACompletes(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{
			SessionID: "sess-1",
			SiteKey:   "site-key",
			RQData:    "rq-data",
		},
		tokens: riot.PasswordTokens{
			AccessToken:   "at-1",
			IDToken:       "id-1",
			SessionCookie: "ssid=s1",
		},
	}
	s := newCaptchaServer(pw)

	url, state, err := s.BeginPasswordLogin(context.Background(), "discord-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if url != "" {
		t.Fatalf("unexpected captcha URL %q", url)
	}

	chReq := newCaptchaRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
	chRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(chRec, chReq)
	if chRec.Code != http.StatusOK {
		t.Fatalf("challenge %d %s", chRec.Code, chRec.Body.String())
	}

	done := make(chan struct{})
	var display, mfaState, mfaHint string
	var waitErr error
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		display, mfaState, mfaHint, waitErr = s.WaitPasswordLogin(ctx, state)
	}()

	subReq := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"hcaptcha-token","version":1}`))
	subReq.Header.Set("Content-Type", "application/json")
	subRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(subRec, subReq)
	if subRec.Code != http.StatusOK {
		t.Fatalf("submit %d %s", subRec.Code, subRec.Body.String())
	}
	var subOut map[string]any
	_ = json.NewDecoder(subRec.Body).Decode(&subOut)
	if subOut["ok"] != true || subOut["mfa"] != false {
		t.Fatalf("submit out %+v", subOut)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("WaitPasswordLogin timeout")
	}
	if waitErr != nil {
		t.Fatal(waitErr)
	}
	if mfaState != "" || mfaHint != "" {
		t.Fatalf("no MFA expected: mfa=%q hint=%q", mfaState, mfaHint)
	}
	if display == "" {
		t.Fatal("expected linked display name")
	}
}

func TestCaptchaChallengeAndSubmit_MFA(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{
			SessionID: "sess-1",
			SiteKey:   "site-key",
			RQData:    "rq-data",
		},
		mfa: &riot.MFAChallenge{
			Email:  "a***@ex.com",
			Method: "email",
		},
	}
	s := newCaptchaServer(pw)

	_, state, err := s.BeginPasswordLogin(context.Background(), "discord-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}

	chReq := newCaptchaRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
	chRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(chRec, chReq)
	if chRec.Code != http.StatusOK {
		t.Fatal(chRec.Body.String())
	}

	done := make(chan struct{})
	var display, mfaState, mfaHint string
	var waitErr error
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		display, mfaState, mfaHint, waitErr = s.WaitPasswordLogin(ctx, state)
	}()

	subReq := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"hcaptcha-token","version":1}`))
	subReq.Header.Set("Content-Type", "application/json")
	subRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(subRec, subReq)
	var subOut map[string]any
	_ = json.NewDecoder(subRec.Body).Decode(&subOut)
	if subOut["ok"] != true || subOut["mfa"] != true {
		t.Fatalf("submit out %+v", subOut)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("WaitPasswordLogin timeout")
	}
	if waitErr != nil {
		t.Fatal(waitErr)
	}
	if display != "" || mfaState == "" || mfaHint != "a***@ex.com" {
		t.Fatalf("display=%q mfa=%q hint=%q", display, mfaState, mfaHint)
	}
}

func TestCaptchaChallengeAndSubmit_MFAScrubsCredentialsBeforePublishing(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{
			SessionID: "sess-1",
			SiteKey:   "site-key",
			RQData:    "rq-data",
		},
		mfa: &riot.MFAChallenge{Email: "a***@ex.com", Method: "email"},
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "sensitive-user", "sensitive-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}

	req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("captcha submit: %d %s", rec.Code, rec.Body.String())
	}

	s.mu.Lock()
	pending, exists := s.passwordPending[state]
	outcome := s.passwordOutcomes[state]
	s.mu.Unlock()
	if !exists || !outcome.done || outcome.mfaState == "" {
		t.Fatalf("MFA publication pending=%v outcome=%+v", exists, outcome)
	}
	if pending.username != "" || pending.password != "" {
		t.Fatalf("MFA publication retained credentials: username=%q passwordBytes=%d", pending.username, len(pending.password))
	}

	_, mfaState, _, err := s.WaitPasswordLogin(context.Background(), state)
	if err != nil || mfaState != outcome.mfaState {
		t.Fatalf("MFA wait state=%q err=%v", mfaState, err)
	}
	s.cleanupMFAState(mfaState)
}

func TestMFAPendingExpiresWithoutFurtherActivity(t *testing.T) {
	pw := &fakePasswordAuth{
		ch:  riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa: &riot.MFAChallenge{Email: "a***@ex.com", Method: "email"},
	}
	s := newCaptchaServer(pw)
	s.pendingTTL = 80 * time.Millisecond
	_, state, err := s.BeginPasswordLogin(context.Background(), "discord-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}

	subReq := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
	subReq.Header.Set("Content-Type", "application/json")
	subRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(subRec, subReq)
	var out map[string]any
	if err := json.NewDecoder(subRec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["mfa"] != true {
		t.Fatalf("submit output = %+v", out)
	}
	_, mfaState, _, err := s.WaitPasswordLogin(context.Background(), state)
	if err != nil || mfaState == "" {
		t.Fatalf("wait mfaState=%q err=%v", mfaState, err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, exists := s.mfaPending[mfaState]
		s.mu.Unlock()
		if !exists {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expired MFA state was retained")
}

func TestMFAExpiryCancelsInflightSubmitBeforeLink(t *testing.T) {
	mfaStarted := make(chan struct{}, 1)
	mfaRelease := make(chan struct{})
	pw := &fakePasswordAuth{
		ch:         riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
		mfa:        &riot.MFAChallenge{Email: "a***@ex.com", Method: "email"},
		mfaStarted: mfaStarted,
		mfaRelease: mfaRelease,
		tokens: riot.PasswordTokens{
			AccessToken:   "at-1",
			IDToken:       "id-1",
			SessionCookie: "ssid=s1",
		},
	}
	s := newCaptchaServer(pw)
	s.pendingTTL = 250 * time.Millisecond
	_, state, err := s.BeginPasswordLogin(context.Background(), "discord-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureCaptchaChallenge(context.Background(), state, testCaptchaBrowserSession()); err != nil {
		t.Fatal(err)
	}
	req := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	_, mfaState, _, err := s.WaitPasswordLogin(context.Background(), state)
	if err != nil || mfaState == "" {
		t.Fatalf("mfa state=%q err=%v", mfaState, err)
	}

	done := make(chan error, 1)
	go func() {
		_, submitErr := s.CompletePasswordMFA(context.Background(), mfaState, "discord-1", "123456")
		done <- submitErr
	}()
	<-mfaStarted
	select {
	case submitErr := <-done:
		if submitErr == nil || !strings.Contains(submitErr.Error(), "expired") {
			t.Fatalf("in-flight MFA error = %v", submitErr)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight MFA was not canceled at expiry")
	}
	st := s.store.(*mockStore)
	if len(st.accounts) != 0 {
		t.Fatalf("expired MFA linked %d accounts", len(st.accounts))
	}
}

func TestCaptchaSubmit_RetryDoesNotFinishWait(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{
			SessionID: "sess-1",
			SiteKey:   "site-key",
			RQData:    "rq-data",
			BrowserCookies: []*http.Cookie{
				{Name: "authenticator.sid", Value: "initial-session", Path: "/", Secure: true, HttpOnly: true},
				{Name: "tdid", Value: "initial-device", Domain: "riotgames.com", Path: "/", Secure: true, HttpOnly: true},
			},
		},
		retryOnce: true,
		retryCookies: []*http.Cookie{
			{Name: "authenticator.sid", Value: "retry-session", Path: "/", Secure: true, HttpOnly: true},
			{Name: "tdid", Value: "", Domain: "riotgames.com", Path: "/", Secure: true, HttpOnly: true, MaxAge: -1},
		},
		mfa: &riot.MFAChallenge{
			Email:  "a***@ex.com",
			Method: "email",
		},
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "discord-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	chReq := newCaptchaRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
	chRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(chRec, chReq)
	if chRec.Code != http.StatusOK {
		t.Fatal(chRec.Body.String())
	}

	waiting := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(waiting)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, _, _ = s.WaitPasswordLogin(ctx, state)
		close(finished)
	}()
	<-waiting
	time.Sleep(50 * time.Millisecond)

	subReq := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"t1","version":1}`))
	subReq.Header.Set("Content-Type", "application/json")
	subRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(subRec, subReq)
	var out map[string]any
	_ = json.NewDecoder(subRec.Body).Decode(&out)
	if out["retry"] != true || out["sitekey"] != "k-retry" {
		t.Fatalf("retry out %+v", out)
	}
	if out["version"] != float64(2) {
		t.Fatalf("retry version = %#v", out["version"])
	}
	retryCookies := subRec.Result().Cookies()
	updatedSession, deletedTDID := false, false
	for _, cookie := range retryCookies {
		if cookie.Name == "authenticator.sid" && cookie.Value == "retry-session" && cookie.Secure && cookie.HttpOnly {
			updatedSession = true
		}
		if cookie.Name == "tdid" && cookie.Value == "" && cookie.MaxAge < 0 && cookie.Domain == "riotgames.com" {
			deletedTDID = true
		}
	}
	if len(retryCookies) != 2 || !updatedSession || !deletedTDID {
		t.Fatalf("retry cookies = %#v", retryCookies)
	}

	recoveryReq := newCaptchaRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
	recoveryRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(recoveryRec, recoveryReq)
	recoveryCookies := recoveryRec.Result().Cookies()
	recoveredSession, recoveredDeletion := false, false
	for _, cookie := range recoveryCookies {
		if cookie.Name == "authenticator.sid" && cookie.Value == "retry-session" {
			recoveredSession = true
		}
		if cookie.Name == "tdid" && cookie.MaxAge < 0 {
			recoveredDeletion = true
		}
	}
	if len(recoveryCookies) != 2 || !recoveredSession || !recoveredDeletion {
		t.Fatalf("recovered challenge cookies = %#v", recoveryCookies)
	}

	staleReq := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"stale","version":1}`))
	staleReq.Header.Set("Content-Type", "application/json")
	staleRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(staleRec, staleReq)
	var staleOut map[string]any
	_ = json.NewDecoder(staleRec.Body).Decode(&staleOut)
	if staleOut["retry"] != true || staleOut["version"] != float64(2) || pw.completeCalls.Load() != 1 {
		t.Fatalf("stale challenge reached Riot: out=%+v calls=%d", staleOut, pw.completeCalls.Load())
	}

	select {
	case <-finished:
		t.Fatal("WaitPasswordLogin must stay open across captcha retry")
	case <-time.After(200 * time.Millisecond):
	}

	subReq2 := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"t2","version":2}`))
	subReq2.Header.Set("Content-Type", "application/json")
	for _, cookie := range retryCookies {
		if cookie.Value != "" && cookie.MaxAge >= 0 {
			subReq2.AddCookie(cookie)
		}
	}
	subRec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(subRec2, subReq2)
	var out2 map[string]any
	_ = json.NewDecoder(subRec2.Body).Decode(&out2)
	if out2["ok"] != true || out2["mfa"] != true {
		t.Fatalf("second submit %+v body=%s", out2, subRec2.Body.String())
	}
	completeBrowser, _ := pw.completeBrowser.Load().(riot.CaptchaBrowserSession)
	if completeBrowser.Cookies["authenticator.sid"] != "retry-session" {
		t.Fatalf("retry completion browser = %#v", completeBrowser)
	}
	if _, ok := completeBrowser.Cookies["tdid"]; ok {
		t.Fatalf("deleted tdid returned on retry completion: %#v", completeBrowser)
	}

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitPasswordLogin did not finish after successful captcha")
	}
}

func TestCaptchaSessionMismatchClearsCookiesAndReloadsChallenge(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{
			SessionID: "sess-reset",
			SiteKey:   "site-key",
			RQData:    "rq-data",
			BrowserCookies: []*http.Cookie{
				{Name: "tdid", Value: "device-1", Domain: "riotgames.com", Path: "/", Secure: true, HttpOnly: true},
			},
		},
		complete: riot.ErrCaptchaSession,
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "discord-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}

	challengeReq := newCaptchaRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
	challengeRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(challengeRec, challengeReq)
	if challengeRec.Code != http.StatusOK {
		t.Fatal(challengeRec.Body.String())
	}

	submitReq := newCaptchaRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
	submitReq.Header.Set("Content-Type", "application/json")
	submitReq.AddCookie(&http.Cookie{Name: "tdid", Value: "stale-device"})
	submitRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(submitRec, submitReq)
	var out map[string]any
	if err := json.NewDecoder(submitRec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["reload"] != true {
		t.Fatalf("session mismatch response = %#v", out)
	}
	cleared := false
	for _, cookie := range submitRec.Result().Cookies() {
		if cookie.Name == "tdid" && cookie.Value == "" && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("session mismatch did not clear browser cookie: %#v", submitRec.Result().Cookies())
	}

	recoveryReq := newCaptchaRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
	recoveryRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(recoveryRec, recoveryReq)
	if recoveryRec.Code != http.StatusOK || pw.beginCalls.Load() != 2 {
		t.Fatalf("recovery status=%d beginCalls=%d body=%s", recoveryRec.Code, pw.beginCalls.Load(), recoveryRec.Body.String())
	}
}
