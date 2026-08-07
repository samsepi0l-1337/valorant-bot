package authweb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
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
	beginDelay      time.Duration
	completeStarted chan struct{}
	completeRelease <-chan struct{}
	completeSession atomic.Value
	canceledSession atomic.Value
	mfaCalls        atomic.Int32
	mfaStarted      chan struct{}
	mfaRelease      <-chan struct{}
}

func (f *fakePasswordAuth) BeginCaptcha(ctx context.Context, username, password string) (riot.CaptchaChallenge, error) {
	f.beginCalls.Add(1)
	if f.beginDelay > 0 {
		select {
		case <-time.After(f.beginDelay):
		case <-ctx.Done():
			return riot.CaptchaChallenge{}, ctx.Err()
		}
	}
	return f.ch, nil
}

func (f *fakePasswordAuth) CompleteCaptcha(ctx context.Context, sessionID, captchaToken string) (riot.PasswordTokens, *riot.MFAChallenge, error) {
	f.completeSession.Store(sessionID)
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
		return riot.PasswordTokens{}, nil, &riot.CaptchaRetryError{
			SiteKey: "k-retry",
			RQData:  "d-retry",
			Reason:  "captcha_not_allowed",
		}
	}
	if f.complete != nil {
		return riot.PasswordTokens{}, nil, f.complete
	}
	return f.tokens, f.mfa, nil
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
	if _, err := s.ensureCaptchaChallenge(context.Background(), state); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
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
			pending, challengeErr := s.ensureCaptchaChallenge(ctx, state)
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

	subReq := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"hcaptcha-token","version":1}`))
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
	if _, err := s.ensureCaptchaChallenge(context.Background(), state); err != nil {
		t.Fatal(err)
	}

	request := func(rec *httptest.ResponseRecorder, done chan<- struct{}) {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
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
	if _, err := s.ensureCaptchaChallenge(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
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
		display, submitErr := s.CompletePasswordMFA(context.Background(), mfaState, "123456")
		results <- result{display: display, err: submitErr}
	}()
	<-mfaStarted
	go func() {
		display, submitErr := s.CompletePasswordMFA(context.Background(), mfaState, "123456")
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
	s.launchCaptchaBrowser = func(string) error { return nil }
	return s
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

func TestLaunchPasswordCaptchaValidatesOwner(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
	}
	s := newCaptchaServer(pw)
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "intruder"); !errors.Is(err, ErrCaptchaOwner) {
		t.Fatalf("intruder launch error = %v, want ErrCaptchaOwner", err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); err != nil {
		t.Fatalf("owner launch: %v", err)
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
	if _, err := s.ensureCaptchaChallenge(ctx, state); err != nil {
		t.Fatal(err)
	}
	_, _, _, waitErr := s.WaitPasswordLogin(ctx, state)
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v", waitErr)
	}
	s.mu.Lock()
	_, pending := s.passwordPending[state]
	_, outcome := s.passwordOutcomes[state]
	_, launched := s.captchaLaunched[state]
	s.mu.Unlock()
	if pending || outcome || launched {
		t.Fatalf("timed-out state retained: pending=%v outcome=%v launched=%v", pending, outcome, launched)
	}
	if got, _ := pw.canceledSession.Load().(string); got != "sess-to-cancel" {
		t.Fatalf("canceled Riot session = %q", got)
	}
}

func TestCaptchaPreparationFailureFinishesWait(t *testing.T) {
	pw := &fakePasswordAuth{
		ch: riot.CaptchaChallenge{SessionID: "sess-1", SiteKey: "site-key", RQData: "rq-data"},
	}
	s := newCaptchaServer(pw)
	s.launchCaptchaBrowser = func(string) error { return errors.New("Chrome/Chromium not found") }
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _, waitErr := s.WaitPasswordLogin(ctx, state)
	if waitErr == nil || !strings.Contains(waitErr.Error(), "Chrome/Chromium") {
		t.Fatalf("preparation error = %v", waitErr)
	}
}

func TestCaptchaWidgetPage_HasCheckbox(t *testing.T) {
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
	if !strings.Contains(body, "로봇이 아닙니다") {
		t.Fatalf("widget should use checkbox copy")
	}
	if strings.Contains(body, "host !== 'auth.riotgames.com'") {
		t.Fatal("widget must reject the legacy Riot OAuth hostname")
	}
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

	chReq := httptest.NewRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
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

	subReq := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"hcaptcha-token","version":1}`))
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

	chReq := httptest.NewRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
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

	subReq := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"hcaptcha-token","version":1}`))
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
	if _, err := s.ensureCaptchaChallenge(context.Background(), state); err != nil {
		t.Fatal(err)
	}

	subReq := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
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
	if _, err := s.ensureCaptchaChallenge(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"token","version":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	_, mfaState, _, err := s.WaitPasswordLogin(context.Background(), state)
	if err != nil || mfaState == "" {
		t.Fatalf("mfa state=%q err=%v", mfaState, err)
	}

	done := make(chan error, 1)
	go func() {
		_, submitErr := s.CompletePasswordMFA(context.Background(), mfaState, "123456")
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
		},
		retryOnce: true,
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
	chReq := httptest.NewRequest(http.MethodGet, "/api/auth/captcha/challenge?state="+state, nil)
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

	subReq := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"t1","version":1}`))
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

	staleReq := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"stale","version":1}`))
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

	subReq2 := httptest.NewRequest(http.MethodPost, "/api/auth/captcha", strings.NewReader(`{"state":"`+state+`","token":"t2","version":2}`))
	subReq2.Header.Set("Content-Type", "application/json")
	subRec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(subRec2, subReq2)
	var out2 map[string]any
	_ = json.NewDecoder(subRec2.Body).Decode(&out2)
	if out2["ok"] != true || out2["mfa"] != true {
		t.Fatalf("second submit %+v body=%s", out2, subRec2.Body.String())
	}

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitPasswordLogin did not finish after successful captcha")
	}
}
