package authweb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

type legacyLifecycleStore struct {
	mu sync.Mutex

	pending  map[string]string
	accounts []store.Account

	putStarted    chan struct{}
	putRelease    <-chan struct{}
	upsertStarted chan struct{}
	upsertRelease <-chan struct{}
}

func newLegacyLifecycleStore() *legacyLifecycleStore {
	return &legacyLifecycleStore{pending: make(map[string]string)}
}

func (s *legacyLifecycleStore) PutAuthPending(state, discordUserID string, _ time.Time) error {
	if s.putStarted != nil {
		select {
		case s.putStarted <- struct{}{}:
		default:
		}
	}
	if s.putRelease != nil {
		<-s.putRelease
	}
	s.mu.Lock()
	s.pending[state] = discordUserID
	s.mu.Unlock()
	return nil
}

func (s *legacyLifecycleStore) TakeAuthPending(state string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	discordUserID, ok := s.pending[state]
	delete(s.pending, state)
	return discordUserID, ok, nil
}

func (s *legacyLifecycleStore) TakeAuthPendingForOwner(state, discordUserID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, ok := s.pending[state]
	if !ok || owner != discordUserID {
		return false, nil
	}
	delete(s.pending, state)
	return true, nil
}

func (s *legacyLifecycleStore) UpsertRiotAccount(account store.Account) error {
	if s.upsertStarted != nil {
		select {
		case s.upsertStarted <- struct{}{}:
		default:
		}
	}
	if s.upsertRelease != nil {
		<-s.upsertRelease
	}
	s.mu.Lock()
	s.accounts = append(s.accounts, account)
	s.mu.Unlock()
	return nil
}

func (s *legacyLifecycleStore) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

type legacyBlockingRiot struct {
	*mockRiot
	entitlementsStarted chan struct{}
	entitlementsRelease <-chan struct{}
}

type lifecycleDrainProbe struct {
	admitted     chan struct{}
	drainEntered chan struct{}
	allowDrain   chan struct{}
	drainPassed  chan struct{}
}

func installLifecycleDrainProbe(s *Server, admissionBlock <-chan struct{}) *lifecycleDrainProbe {
	probe := &lifecycleDrainProbe{
		admitted:     make(chan struct{}),
		drainEntered: make(chan struct{}),
		allowDrain:   make(chan struct{}),
		drainPassed:  make(chan struct{}),
	}
	var admittedOnce sync.Once
	s.afterLifecycleAdmissionForTest = func() {
		admittedOnce.Do(func() { close(probe.admitted) })
		if admissionBlock != nil {
			<-admissionBlock
		}
	}
	s.beforeLifecycleDrainForTest = func() {
		close(probe.drainEntered)
		<-probe.allowDrain
	}
	s.afterLifecycleDrainForTest = func() { close(probe.drainPassed) }
	return probe
}

func waitLifecycleSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func assertLifecycleDrainHeld(t *testing.T, probe *lifecycleDrainProbe) {
	t.Helper()
	waitLifecycleSignal(t, probe.drainEntered, "Shutdown lifecycle drain entry")
	close(probe.allowDrain)
	select {
	case <-probe.drainPassed:
		t.Fatal("Shutdown passed lifecycle drain while the admitted operation remained blocked")
	case <-time.After(100 * time.Millisecond):
	}
}

func (r *legacyBlockingRiot) GetEntitlements(ctx context.Context, accessToken string) (string, error) {
	select {
	case r.entitlementsStarted <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-r.entitlementsRelease:
		return r.mockRiot.GetEntitlements(ctx, accessToken)
	}
}

// Mutation caught: removing the closed-gate admission check from BeginAuth lets
// a post-shutdown caller persist a new pending state and publish an outcome.
func TestBeginAuthRejectsAfterShutdownWithoutState(t *testing.T) {
	st := newLegacyLifecycleStore()
	s := New(testDeps(st, &mockRiot{}, &mockBoxer{}))
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.BeginAuth("owner-1"); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("BeginAuth error=%v, want ErrServerClosed", err)
	}
	if got := st.pendingCount(); got != 0 {
		t.Fatalf("persisted pending states=%d, want 0", got)
	}
	s.mu.Lock()
	outcomeCount := len(s.outcomes)
	s.mu.Unlock()
	if outcomeCount != 0 {
		t.Fatalf("in-memory outcomes=%d, want 0", outcomeCount)
	}
}

// Mutation caught: removing BeginAuth lifecycle enrollment or Shutdown's join
// lets the drain boundary pass while PutAuthPending is still blocked.
func TestLifecycleDrainWaitsForAdmittedBeginAuth(t *testing.T) {
	putRelease := make(chan struct{})
	st := newLegacyLifecycleStore()
	st.putStarted = make(chan struct{}, 1)
	st.putRelease = putRelease
	s := New(testDeps(st, &mockRiot{}, &mockBoxer{}))
	probe := installLifecycleDrainProbe(s, nil)

	beginDone := make(chan error, 1)
	go func() {
		_, _, err := s.BeginAuth("owner-1")
		beginDone <- err
	}()
	waitLifecycleSignal(t, probe.admitted, "BeginAuth lifecycle admission")
	waitLifecycleSignal(t, st.putStarted, "BeginAuth persisted-state write")
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	assertLifecycleDrainHeld(t, probe)
	close(putRelease)

	select {
	case err := <-beginDone:
		if !errors.Is(err, ErrServerClosed) {
			t.Fatalf("BeginAuth error=%v, want ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("BeginAuth did not finish after release")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after BeginAuth drained")
	}
}

// Mutation caught: removing BeginAuth lifecycle enrollment or its post-persist
// rollback lets Shutdown return while PutAuthPending later leaks both states.
func TestShutdownJoinsConcurrentBeginAuthAndRollsBackPendingState(t *testing.T) {
	putRelease := make(chan struct{})
	st := newLegacyLifecycleStore()
	st.putStarted = make(chan struct{}, 1)
	st.putRelease = putRelease
	s := New(testDeps(st, &mockRiot{}, &mockBoxer{}))
	closedBoundary := make(chan struct{})
	s.afterShutdownClosedForTest = func() { close(closedBoundary) }

	beginDone := make(chan error, 1)
	go func() {
		_, _, err := s.BeginAuth("owner-1")
		beginDone <- err
	}()
	select {
	case <-st.putStarted:
	case <-time.After(time.Second):
		close(putRelease)
		t.Fatal("BeginAuth did not reach persisted-state creation")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case <-closedBoundary:
	case <-time.After(time.Second):
		close(putRelease)
		t.Fatal("Shutdown did not publish its closed boundary")
	}
	close(putRelease)

	select {
	case err := <-beginDone:
		if !errors.Is(err, ErrServerClosed) {
			t.Errorf("BeginAuth error=%v, want ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("BeginAuth did not leave after persisted-state release")
	}
	select {
	case shutdownErr := <-shutdownDone:
		if shutdownErr != nil {
			t.Errorf("Shutdown error=%v", shutdownErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not join concurrent BeginAuth")
	}
	if got := st.pendingCount(); got != 0 {
		t.Errorf("persisted pending states=%d, want 0", got)
	}
	s.mu.Lock()
	outcomeCount := len(s.outcomes)
	s.mu.Unlock()
	if outcomeCount != 0 {
		t.Errorf("in-memory outcomes=%d, want 0", outcomeCount)
	}
}

// Mutation caught: making WaitBrowserLogin observe only the caller context
// leaves the waiter blocked after the server lifecycle has been canceled.
func TestWaitBrowserLoginWakesOnShutdownWithServerClosed(t *testing.T) {
	s := New(testDeps(newLegacyLifecycleStore(), &mockRiot{}, &mockBoxer{}))
	s.setOutcome("legacy-state", authOutcome{Done: false})
	callerCtx, callerCancel := context.WithCancel(context.Background())
	defer callerCancel()
	waitStarted := make(chan struct{})
	waitDone := make(chan error, 1)
	go func() {
		close(waitStarted)
		_, err := s.WaitBrowserLogin(callerCtx, "legacy-state")
		waitDone <- err
	}()
	<-waitStarted

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitDone:
		if !errors.Is(err, ErrServerClosed) {
			t.Fatalf("WaitBrowserLogin error=%v, want ErrServerClosed", err)
		}
	case <-time.After(200 * time.Millisecond):
		callerCancel()
		<-waitDone
		t.Fatal("WaitBrowserLogin remained blocked on its caller context after shutdown")
	}
}

// Mutation caught: signaling before WaitBrowserLogin admission, omitting its
// enrollment, or omitting Shutdown's join lets the drain pass this held method.
func TestLifecycleDrainWaitsForAdmittedWaitBrowserLogin(t *testing.T) {
	s := New(testDeps(newLegacyLifecycleStore(), &mockRiot{}, &mockBoxer{}))
	s.setOutcome("legacy-state", authOutcome{Done: false})
	admissionRelease := make(chan struct{})
	probe := installLifecycleDrainProbe(s, admissionRelease)
	waitDone := make(chan error, 1)
	go func() {
		_, err := s.WaitBrowserLogin(context.Background(), "legacy-state")
		waitDone <- err
	}()
	waitLifecycleSignal(t, probe.admitted, "WaitBrowserLogin lifecycle admission")
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	assertLifecycleDrainHeld(t, probe)
	close(admissionRelease)

	select {
	case err := <-waitDone:
		if !errors.Is(err, ErrServerClosed) {
			t.Fatalf("WaitBrowserLogin error=%v, want ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitBrowserLogin did not finish after admission release")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after WaitBrowserLogin drained")
	}
}

// Mutation caught: bypassing lifecycle context in CompleteFromRedirectURL lets
// Riot identity work survive shutdown and prevents Shutdown from joining it.
func TestShutdownCancelsAndJoinsCompleteFromRedirectURL(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	st := newLegacyLifecycleStore()
	st.pending["legacy-state"] = "owner-1"
	ri := &legacyBlockingRiot{
		mockRiot:            &mockRiot{entitlements: "entitlements"},
		entitlementsStarted: make(chan struct{}, 1),
		entitlementsRelease: release,
	}
	s := New(testDeps(st, ri, &mockBoxer{}))
	redirectURL := "http://localhost/redirect#access_token=" + jwtAccessToken("kr") +
		"&id_token=" + jwtIDToken("Legacy", "KR1") + "&state=legacy-state"

	completeDone := make(chan error, 1)
	go func() {
		_, err := s.CompleteFromRedirectURL(context.Background(), "legacy-state", redirectURL, "")
		completeDone <- err
	}()
	select {
	case <-ri.entitlementsStarted:
	case <-time.After(time.Second):
		t.Fatal("redirect completion did not reach Riot identity lookup")
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completeDone:
		if !errors.Is(err, ErrServerClosed) {
			t.Fatalf("CompleteFromRedirectURL error=%v, want ErrServerClosed", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Shutdown returned without canceling and joining redirect completion")
	}
	if got := st.pendingCount(); got != 0 {
		t.Fatalf("consumed redirect pending states=%d, want 0", got)
	}
}

// Mutation caught: ending callback enrollment before outcome publication lets
// Shutdown return while a claimed store commit later repopulates outcomes.
func TestShutdownJoinsFullCallbackAndPreventsPostCloseOutcome(t *testing.T) {
	upsertRelease := make(chan struct{})
	st := newLegacyLifecycleStore()
	st.pending["legacy-state"] = "owner-1"
	st.upsertStarted = make(chan struct{}, 1)
	st.upsertRelease = upsertRelease
	ri := &mockRiot{
		entitlements: "entitlements",
		puuid:        "puuid-1",
		names:        []riot.PlayerName{{GameName: "Legacy", TagLine: "KR1"}},
		region:       "kr",
		shard:        "kr",
	}
	s := New(testDeps(st, ri, &mockBoxer{}))
	closedBoundary := make(chan struct{})
	s.afterShutdownClosedForTest = func() { close(closedBoundary) }
	s.setOutcome("legacy-state", authOutcome{Done: false})
	redirectURL := "http://localhost/redirect#access_token=" + jwtAccessToken("kr") +
		"&id_token=" + jwtIDToken("Legacy", "KR1") + "&state=legacy-state"
	form := url.Values{"state": {"legacy-state"}, "redirect_url": {redirectURL}}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(rec, req)
		close(handlerDone)
	}()
	select {
	case <-st.upsertStarted:
	case <-time.After(time.Second):
		close(upsertRelease)
		t.Fatal("callback did not reach account commit")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case <-closedBoundary:
	case <-time.After(time.Second):
		close(upsertRelease)
		t.Fatal("Shutdown did not publish its closed boundary")
	}
	close(upsertRelease)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("callback did not leave after account commit release")
	}
	select {
	case shutdownErr := <-shutdownDone:
		if shutdownErr != nil {
			t.Errorf("Shutdown error=%v", shutdownErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not join the callback")
	}
	s.mu.Lock()
	_, outcomeExists := s.outcomes["legacy-state"]
	s.mu.Unlock()
	if outcomeExists {
		t.Error("callback inserted an outcome after the server closed boundary")
	}
}

// Mutation caught: ending callback lifecycle enrollment before its blocked
// account commit, or omitting Shutdown's join, crosses the drain too early.
func TestLifecycleDrainWaitsForAdmittedCallback(t *testing.T) {
	upsertRelease := make(chan struct{})
	st := newLegacyLifecycleStore()
	st.pending["legacy-state"] = "owner-1"
	st.upsertStarted = make(chan struct{}, 1)
	st.upsertRelease = upsertRelease
	ri := &mockRiot{
		entitlements: "entitlements",
		puuid:        "puuid-1",
		names:        []riot.PlayerName{{GameName: "Legacy", TagLine: "KR1"}},
		region:       "kr",
		shard:        "kr",
	}
	s := New(testDeps(st, ri, &mockBoxer{}))
	probe := installLifecycleDrainProbe(s, nil)
	s.setOutcome("legacy-state", authOutcome{Done: false})
	redirectURL := "http://localhost/redirect#access_token=" + jwtAccessToken("kr") +
		"&id_token=" + jwtIDToken("Legacy", "KR1") + "&state=legacy-state"
	form := url.Values{"state": {"legacy-state"}, "redirect_url": {redirectURL}}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(rec, req)
		close(handlerDone)
	}()
	waitLifecycleSignal(t, probe.admitted, "callback lifecycle admission")
	waitLifecycleSignal(t, st.upsertStarted, "callback account commit")
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	assertLifecycleDrainHeld(t, probe)
	close(upsertRelease)

	waitLifecycleSignal(t, handlerDone, "callback completion")
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after callback drained")
	}
}

// Mutation caught: testing setOutcome only after Shutdown clears the map does
// not detect removal of its closed publication gate. Exercise the gate itself.
func TestSetOutcomeRejectsAfterClosedBoundary(t *testing.T) {
	s := New(testDeps(newLegacyLifecycleStore(), &mockRiot{}, &mockBoxer{}))
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	if accepted := s.setOutcome("late-state", authOutcome{Done: true, OK: true, Display: "Late#1"}); accepted {
		t.Fatal("setOutcome accepted publication after closed boundary")
	}
	s.mu.Lock()
	_, exists := s.outcomes["late-state"]
	s.mu.Unlock()
	if exists {
		t.Fatal("setOutcome inserted an outcome after closed boundary")
	}
}
