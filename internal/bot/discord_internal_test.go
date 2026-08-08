package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/authweb"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

type acknowledgementCheckingAuth struct {
	acknowledged    *atomic.Bool
	beginBeforeACK  atomic.Bool
	launchBeforeACK atomic.Bool
	launchStarted   chan<- struct{}
	launchRelease   <-chan struct{}
	waitStarted     chan<- struct{}
	waitRelease     <-chan struct{}
	waitDone        chan<- struct{}
}

type passwordButtonAuth struct {
	launchErr            error
	launches             atomic.Int32
	waits                atomic.Int32
	waitStarted          chan<- struct{}
	waitRelease          <-chan struct{}
	waitDone             chan<- struct{}
	launchEditResponded  *atomic.Bool
	waitBeforeLaunchEdit atomic.Bool
	cancelCalls          atomic.Int32
	cancelState          string
	cancelUser           string
}

type mfaInteractionAuth struct {
	mu                   sync.Mutex
	validateState        string
	validateUser         string
	validateHint         string
	validateErr          error
	validateCalls        atomic.Int32
	completeState        string
	completeUser         string
	completeCode         string
	completeCalls        atomic.Int32
	completeBeforeACK    atomic.Bool
	acknowledged         *atomic.Bool
	firstCompleteStarted chan<- struct{}
	firstCompleteRelease <-chan struct{}
	completeResults      []mfaCompletionResult
	completeDisplay      string
	completeErr          error
}

type mfaCompletionResult struct {
	display string
	err     error
}

type mfaLanguageOrderStore struct {
	auth                 *mfaInteractionAuth
	readBeforeValidation atomic.Bool
}

type blockingLanguageStore struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

type ackLanguageStore struct {
	acknowledged  *atomic.Bool
	readBeforeACK atomic.Bool
}

type acknowledgedWishlistStore struct {
	acknowledged *atomic.Bool
	beforeACK    atomic.Bool
}

func (s *acknowledgedWishlistStore) AddWishlist(string, string, string) error {
	if !s.acknowledged.Load() {
		s.beforeACK.Store(true)
	}
	return nil
}

func (*acknowledgedWishlistStore) RemoveWishlist(string, string) error { return nil }

func (*acknowledgedWishlistStore) ListWishlists(string) ([]store.WishlistItem, error) {
	return nil, nil
}

type acknowledgedSkinStore struct {
	acknowledged *atomic.Bool
	beforeACK    atomic.Bool
}

func (s *acknowledgedSkinStore) EnsureLoaded(context.Context, string) error {
	if !s.acknowledged.Load() {
		s.beforeACK.Store(true)
	}
	return nil
}

func (*acknowledgedSkinStore) SearchByName(string, string) []skins.Skin { return nil }

func (s *acknowledgedSkinStore) Get(uuid, _ string) (skins.Skin, bool) {
	if !s.acknowledged.Load() {
		s.beforeACK.Store(true)
	}
	return skins.Skin{UUID: uuid, DisplayName: "Prime Vandal"}, true
}

type acknowledgedGuildStore struct {
	acknowledged *atomic.Bool
	beforeACK    atomic.Bool
}

func (s *acknowledgedGuildStore) GetGuildSettings(guildID string) (store.GuildSettings, bool, error) {
	if !s.acknowledged.Load() {
		s.beforeACK.Store(true)
	}
	return store.GuildSettings{GuildID: guildID, DailyChannelID: "channel-1", Enabled: true, DailyHour: 9}, true, nil
}

func (s *acknowledgedGuildStore) UpsertGuildSettings(store.GuildSettings) error {
	if !s.acknowledged.Load() {
		s.beforeACK.Store(true)
	}
	return nil
}

func (s *ackLanguageStore) GetUserLanguage(string) (string, error) {
	if !s.acknowledged.Load() {
		s.readBeforeACK.Store(true)
	}
	return string(i18n.KO), nil
}

func (*ackLanguageStore) SetUserLanguage(string, string) error { return nil }

type cancelingWatcherAuth struct {
	mfaInteractionAuth
	qrStarted       chan struct{}
	passwordStarted chan struct{}
}

type lifecycleInteractionAuth struct {
	mfaInteractionAuth
	started  chan struct{}
	release  <-chan struct{}
	calls    atomic.Int32
	canceled atomic.Bool
}

type consumedAfterSuccessMFAAuth struct {
	mfaInteractionAuth
}

type mfaDeliveryAuth struct {
	mfaInteractionAuth
	waitMFAState    string
	waitMFAHint     string
	waitErr         error
	cancelErr       error
	cancelCalls     atomic.Int32
	cancelSucceeded atomic.Bool
	cancelMu        sync.Mutex
	cancelState     string
	cancelUser      string
}

func (a *mfaDeliveryAuth) WaitPasswordLogin(context.Context, string) (string, string, string, error) {
	return "", a.waitMFAState, a.waitMFAHint, a.waitErr
}

func (a *mfaDeliveryAuth) CancelPasswordMFA(mfaState, discordUserID string) error {
	a.cancelCalls.Add(1)
	a.cancelMu.Lock()
	a.cancelState = mfaState
	a.cancelUser = discordUserID
	a.cancelMu.Unlock()
	if a.cancelErr != nil {
		return a.cancelErr
	}
	a.cancelSucceeded.Store(true)
	return nil
}

func (a *consumedAfterSuccessMFAAuth) ValidatePasswordMFA(mfaState, discordUserID string) (string, error) {
	a.validateCalls.Add(1)
	if a.completeCalls.Load() != 0 {
		return "", authweb.ErrMFAExpired
	}
	return "", nil
}

func (a *lifecycleInteractionAuth) BeginPasswordLogin(ctx context.Context, _, _, _ string) (string, string, error) {
	a.calls.Add(1)
	select {
	case a.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		a.canceled.Store(true)
		return "", "", ctx.Err()
	case <-a.release:
		return "", "captcha-state", nil
	}
}

func (a *cancelingWatcherAuth) WaitQRLogin(ctx context.Context, _ string) (string, error) {
	close(a.qrStarted)
	<-ctx.Done()
	return "", ctx.Err()
}

func (a *cancelingWatcherAuth) WaitPasswordLogin(ctx context.Context, _ string) (string, string, string, error) {
	close(a.passwordStarted)
	<-ctx.Done()
	return "", "", "", ctx.Err()
}

func (s *blockingLanguageStore) GetUserLanguage(string) (string, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return string(i18n.EN), nil
}

func (*blockingLanguageStore) SetUserLanguage(string, string) error { return nil }

func (s *mfaLanguageOrderStore) GetUserLanguage(string) (string, error) {
	if s.auth.validateCalls.Load() == 0 {
		s.readBeforeValidation.Store(true)
	}
	return string(i18n.KO), nil
}

func (*mfaLanguageOrderStore) SetUserLanguage(string, string) error { return nil }

func (*mfaInteractionAuth) BeginQRAuth(context.Context, string) (string, string, error) {
	return "", "", nil
}

func (*mfaInteractionAuth) WaitQRLogin(context.Context, string) (string, error) {
	return "", nil
}

func (*mfaInteractionAuth) BeginPasswordLogin(context.Context, string, string, string) (string, string, error) {
	return "", "", nil
}

func (*mfaInteractionAuth) LaunchPasswordCaptcha(context.Context, string, string) error {
	return nil
}

func (*mfaInteractionAuth) WaitPasswordLogin(context.Context, string) (string, string, string, error) {
	return "", "", "", nil
}

func (a *mfaInteractionAuth) ValidatePasswordMFA(mfaState, discordUserID string) (string, error) {
	a.validateCalls.Add(1)
	a.mu.Lock()
	a.validateState = mfaState
	a.validateUser = discordUserID
	a.mu.Unlock()
	return a.validateHint, a.validateErr
}

func (a *mfaInteractionAuth) CompletePasswordMFA(_ context.Context, mfaState, discordUserID, code string) (string, error) {
	call := int(a.completeCalls.Add(1))
	if a.acknowledged != nil && !a.acknowledged.Load() {
		a.completeBeforeACK.Store(true)
	}
	a.mu.Lock()
	a.completeState = mfaState
	a.completeUser = discordUserID
	a.completeCode = code
	a.mu.Unlock()
	if call == 1 && a.firstCompleteStarted != nil {
		a.firstCompleteStarted <- struct{}{}
	}
	if call == 1 && a.firstCompleteRelease != nil {
		<-a.firstCompleteRelease
	}
	if call <= len(a.completeResults) {
		result := a.completeResults[call-1]
		return result.display, result.err
	}
	return a.completeDisplay, a.completeErr
}

func (*mfaInteractionAuth) CancelPasswordMFA(string, string) error { return nil }

type capturedInteractionResponse struct {
	Type discordgo.InteractionResponseType `json:"type"`
	Data struct {
		Content    string                 `json:"content"`
		CustomID   string                 `json:"custom_id"`
		Flags      discordgo.MessageFlags `json:"flags"`
		Components []json.RawMessage      `json:"components"`
	} `json:"data"`
}

func newInteractionResponseCapture(t *testing.T) (*discordgo.Session, <-chan capturedInteractionResponse) {
	t.Helper()
	responses := make(chan capturedInteractionResponse, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		var response capturedInteractionResponse
		if err := json.NewDecoder(r.Body).Decode(&response); err != nil {
			t.Errorf("decode callback: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		responses <- response
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	originalCallbackEndpoint := discordgo.EndpointInteractionResponse
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	t.Cleanup(func() { discordgo.EndpointInteractionResponse = originalCallbackEndpoint })

	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	return session, responses
}

type capturedInteractionEdit struct {
	Content    string `json:"content"`
	Components []struct {
		Components []struct {
			CustomID string `json:"custom_id"`
		} `json:"components"`
	} `json:"components"`
}

type mfaInteractionCapture struct {
	responses <-chan capturedInteractionResponse
	edits     <-chan capturedInteractionEdit
}

func newMFAInteractionCapture(t *testing.T, acknowledged *atomic.Bool) (*discordgo.Session, mfaInteractionCapture) {
	t.Helper()
	responses := make(chan capturedInteractionResponse, 8)
	edits := make(chan capturedInteractionEdit, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/callback":
			var response capturedInteractionResponse
			if err := json.NewDecoder(r.Body).Decode(&response); err != nil {
				t.Errorf("decode callback: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if acknowledged != nil {
				acknowledged.Store(true)
			}
			responses <- response
			w.WriteHeader(http.StatusNoContent)
		case "/original":
			var edit capturedInteractionEdit
			if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
				t.Errorf("decode original edit: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			edits <- edit
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	originalCallbackEndpoint := discordgo.EndpointInteractionResponse
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return srv.URL + "/original" }
	t.Cleanup(func() {
		discordgo.EndpointInteractionResponse = originalCallbackEndpoint
		discordgo.EndpointWebhookMessage = originalWebhookEndpoint
	})

	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	return session, mfaInteractionCapture{responses: responses, edits: edits}
}

func newFailedOriginalEditSession(t *testing.T) *discordgo.Session {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/callback":
			w.WriteHeader(http.StatusNoContent)
		case "/original":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"edit rejected","code":40060}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	originalCallbackEndpoint := discordgo.EndpointInteractionResponse
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return srv.URL + "/original" }
	t.Cleanup(func() {
		discordgo.EndpointInteractionResponse = originalCallbackEndpoint
		discordgo.EndpointWebhookMessage = originalWebhookEndpoint
	})

	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func mfaModalSubmitInteraction(id, userID, state, code string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    id,
		AppID: "application-1",
		Token: id + "-token",
		Type:  discordgo.InteractionModalSubmit,
		Data: discordgo.ModalSubmitInteractionData{
			CustomID: customIDAuthMFAPref + state,
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.TextInput{CustomID: "code", Value: code},
			}}},
		},
		User: &discordgo.User{ID: userID},
	}}
}

func (*passwordButtonAuth) BeginQRAuth(context.Context, string) (string, string, error) {
	return "", "", nil
}

func (*passwordButtonAuth) WaitQRLogin(context.Context, string) (string, error) {
	return "", nil
}

func (*passwordButtonAuth) BeginPasswordLogin(context.Context, string, string, string) (string, string, error) {
	return "", "captcha-state", nil
}

func (a *passwordButtonAuth) LaunchPasswordCaptcha(context.Context, string, string) error {
	a.launches.Add(1)
	return a.launchErr
}

func (a *passwordButtonAuth) CancelPasswordLogin(state, discordUserID string) error {
	a.cancelCalls.Add(1)
	a.cancelState = state
	a.cancelUser = discordUserID
	return nil
}

func (a *passwordButtonAuth) WaitPasswordLogin(context.Context, string) (string, string, string, error) {
	a.waits.Add(1)
	if a.launchEditResponded != nil && !a.launchEditResponded.Load() {
		a.waitBeforeLaunchEdit.Store(true)
	}
	if a.waitStarted != nil {
		a.waitStarted <- struct{}{}
	}
	<-a.waitRelease
	if a.waitDone != nil {
		a.waitDone <- struct{}{}
	}
	return "", "", "", context.Canceled
}

func TestCaptchaWatcherStartsAfterLaunchStatusAndCannotBeOverwritten(t *testing.T) {
	var launchEditResponded atomic.Bool
	waitStarted := make(chan struct{}, 1)
	waitRelease := make(chan struct{})
	close(waitRelease)
	auth := &passwordButtonAuth{
		waitStarted:         waitStarted,
		waitRelease:         waitRelease,
		launchEditResponded: &launchEditResponded,
	}
	var (
		mu       sync.Mutex
		contents []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/callback":
			w.WriteHeader(http.StatusNoContent)
		case "/original":
			var edit capturedInteractionEdit
			if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
				t.Errorf("decode edit: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if edit.Content == i18n.T(i18n.KO, "auth.captcha.launched") {
				select {
				case <-waitStarted:
				case <-time.After(75 * time.Millisecond):
				}
				launchEditResponded.Store(true)
			}
			mu.Lock()
			contents = append(contents, edit.Content)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	originalCallbackEndpoint := discordgo.EndpointInteractionResponse
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return srv.URL + "/original" }
	t.Cleanup(func() {
		discordgo.EndpointInteractionResponse = originalCallbackEndpoint
		discordgo.EndpointWebhookMessage = originalWebhookEndpoint
	})
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{Auth: auth}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-captcha-order",
		AppID: "application-1",
		Token: "token-captcha-order",
		Type:  discordgo.InteractionMessageComponent,
		Data:  discordgo.MessageComponentInteractionData{CustomID: customIDAuthCaptchaPref + "captcha-state"},
		User:  &discordgo.User{ID: "owner-1"},
	}}
	h.onComponent(session, interaction)
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		count := len(contents)
		last := ""
		if count > 0 {
			last = contents[count-1]
		}
		mu.Unlock()
		if count >= 2 {
			if auth.waitBeforeLaunchEdit.Load() {
				t.Fatal("CAPTCHA watcher began before the launch status edit completed")
			}
			if last == i18n.T(i18n.KO, "auth.captcha.launched") {
				t.Fatalf("launch status overwrote terminal watcher edit: %q", last)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CAPTCHA edits=%v, want launch then terminal", contents)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCaptchaTerminalEditSuppressesConcurrentReopenStatus(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	releaseTerminal := make(chan struct{})
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/original" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		requestStarted <- struct{}{}
		<-releaseTerminal
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return srv.URL + "/original" }
	t.Cleanup(func() { discordgo.EndpointWebhookMessage = originalWebhookEndpoint })
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "interaction-terminal-race", AppID: "application-1", Token: "token-terminal-race",
	}}
	terminalDone := make(chan error, 1)
	go func() {
		terminalDone <- h.editCaptchaInteraction(session, interaction, "captcha-state", Response{
			Content:    "terminal",
			Components: []discordgo.MessageComponent{},
		}, true)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("terminal edit did not start")
	}
	reopenDone := make(chan error, 1)
	go func() {
		reopenDone <- h.editCaptchaInteraction(session, interaction, "captcha-state", Response{Content: "launched"}, false)
	}()
	select {
	case err := <-reopenDone:
		t.Fatalf("reopen edit bypassed in-flight terminal edit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseTerminal)
	for _, done := range []<-chan error{terminalDone, reopenDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("serialized edit did not finish")
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("Discord edits=%d, terminal success should suppress reopen", got)
	}
}

// Mutation caught: holding captchaEditGuard's mutex while Discord's webhook is
// blocked violates lock/I/O separation even if the higher-level edit is serialized.
func TestCaptchaEditDoesNotHoldGuardMutexAcrossDiscordIO(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	releaseRequest := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestStarted <- struct{}{}
		<-releaseRequest
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return srv.URL + "/original" }
	t.Cleanup(func() { discordgo.EndpointWebhookMessage = originalWebhookEndpoint })
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "interaction-guard-io", AppID: "application-1", Token: "token-guard-io",
	}}
	editDone := make(chan error, 1)
	go func() {
		editDone <- h.editCaptchaInteraction(session, interaction, "captcha-state", Response{Content: "terminal"}, true)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		close(releaseRequest)
		t.Fatal("Discord edit did not start")
	}
	guard := h.captchaEditGuard("captcha-state")
	if !guard.TryLock() {
		close(releaseRequest)
		<-editDone
		t.Fatal("captcha edit guard mutex was held across Discord I/O")
	}
	guard.Unlock()
	close(releaseRequest)
	if err := <-editDone; err != nil {
		t.Fatal(err)
	}
}

func TestCaptchaTerminalEditSuppressesLateReopenWatcher(t *testing.T) {
	waitStarted := make(chan struct{}, 1)
	waitRelease := make(chan struct{})
	auth := &passwordButtonAuth{
		waitStarted: waitStarted,
		waitRelease: waitRelease,
	}
	session, capture := newMFAInteractionCapture(t, nil)
	h := &Handlers{Auth: auth}
	t.Cleanup(func() {
		close(waitRelease)
		_ = h.Shutdown(context.Background())
	})
	guard := h.captchaEditGuard("captcha-state")
	guard.Lock()
	guard.terminal = true
	guard.Unlock()
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-terminal-reopen",
		AppID: "application-1",
		Token: "token-terminal-reopen",
		Type:  discordgo.InteractionMessageComponent,
		Data: discordgo.MessageComponentInteractionData{
			CustomID: customIDAuthCaptchaPref + "captcha-state",
		},
		User: &discordgo.User{ID: "owner-1"},
	}}

	h.onComponent(session, interaction)
	select {
	case response := <-capture.responses:
		if response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
			t.Fatalf("captcha reopen ACK type=%d, want deferred source update", response.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("captcha reopen was not acknowledged")
	}
	select {
	case <-waitStarted:
		t.Fatal("terminal-suppressed captcha reopen started a replacement watcher")
	case <-time.After(100 * time.Millisecond):
	}
}

func (*passwordButtonAuth) ValidatePasswordMFA(string, string) (string, error) {
	return "", nil
}

func (*passwordButtonAuth) CompletePasswordMFA(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (*passwordButtonAuth) CancelPasswordMFA(string, string) error { return nil }

func (a *acknowledgementCheckingAuth) BeginQRAuth(context.Context, string) (string, string, error) {
	if !a.acknowledged.Load() {
		a.beginBeforeACK.Store(true)
	}
	return "https://qrlogin.riotgames.com/riotmobile?suuid=s1", "", nil
}

func (*acknowledgementCheckingAuth) WaitQRLogin(context.Context, string) (string, error) {
	return "", nil
}

func (*acknowledgementCheckingAuth) BeginPasswordLogin(context.Context, string, string, string) (string, string, error) {
	return "", "", nil
}

func (a *acknowledgementCheckingAuth) LaunchPasswordCaptcha(context.Context, string, string) error {
	if !a.acknowledged.Load() {
		a.launchBeforeACK.Store(true)
	}
	if a.launchStarted != nil {
		a.launchStarted <- struct{}{}
	}
	if a.launchRelease != nil {
		<-a.launchRelease
	}
	return nil
}

func (a *acknowledgementCheckingAuth) WaitPasswordLogin(context.Context, string) (string, string, string, error) {
	if a.waitStarted != nil {
		a.waitStarted <- struct{}{}
	}
	if a.waitRelease != nil {
		<-a.waitRelease
	}
	if a.waitDone != nil {
		a.waitDone <- struct{}{}
	}
	return "", "", "", context.Canceled
}

func (*acknowledgementCheckingAuth) ValidatePasswordMFA(string, string) (string, error) {
	return "", nil
}

func (*acknowledgementCheckingAuth) CompletePasswordMFA(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (*acknowledgementCheckingAuth) CancelPasswordMFA(string, string) error { return nil }

func TestMFAOpenComponentPassesOwnerToValidation(t *testing.T) {
	session, responses := newInteractionResponseCapture(t)
	auth := &mfaInteractionAuth{validateHint: "a***@example.com"}
	h := &Handlers{Auth: auth}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-mfa-open",
		AppID: "application-1",
		Token: "token-mfa-open",
		Type:  discordgo.InteractionMessageComponent,
		Data: discordgo.MessageComponentInteractionData{
			CustomID: customIDAuthMFAOpenPref + "mfa-state-1",
		},
		User: &discordgo.User{ID: "owner-1"},
	}}

	h.onComponent(session, interaction)
	response := <-responses
	if auth.validateState != "mfa-state-1" || auth.validateUser != "owner-1" {
		t.Fatalf("MFA validation got state=%q user=%q", auth.validateState, auth.validateUser)
	}
	if response.Type != discordgo.InteractionResponseModal || response.Data.CustomID != customIDAuthMFAPref+"mfa-state-1" {
		t.Fatalf("MFA open response=%+v", response)
	}
}

func TestPasswordModalOpenUsesCachedLanguageWithoutDatabaseRead(t *testing.T) {
	session, responses := newInteractionResponseCapture(t)
	release := make(chan struct{})
	lang := &blockingLanguageStore{started: make(chan struct{}), release: release}
	h := &Handlers{Lang: lang}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-password-open",
		AppID: "application-1",
		Token: "token-password-open",
		Type:  discordgo.InteractionMessageComponent,
		Data:  discordgo.MessageComponentInteractionData{CustomID: customIDAuthPassword},
		User:  &discordgo.User{ID: "owner-1"},
	}}
	done := make(chan struct{})
	go func() {
		h.onComponent(session, interaction)
		close(done)
	}()
	select {
	case response := <-responses:
		if response.Type != discordgo.InteractionResponseModal {
			t.Fatalf("password open response=%+v", response)
		}
	case <-lang.started:
		close(release)
		<-done
		t.Fatal("password modal opening read the language database before ACK")
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("password modal opening did not respond")
	}
	close(release)
	<-done
}

func TestPasswordModalSubmitAcknowledgesBeforeLanguageDatabaseAndAuthWork(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	release := make(chan struct{})
	lang := &blockingLanguageStore{started: make(chan struct{}), release: release}
	h := &Handlers{Auth: &passwordButtonAuth{}, Lang: lang}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-password-submit-order",
		AppID: "application-1",
		Token: "token-password-submit-order",
		Type:  discordgo.InteractionModalSubmit,
		Data: discordgo.ModalSubmitInteractionData{
			CustomID: customIDAuthPWModal,
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.TextInput{CustomID: "username", Value: "user"},
				discordgo.TextInput{CustomID: "password", Value: "pass"},
			}}},
		},
		User: &discordgo.User{ID: "owner-1"},
	}}
	done := make(chan struct{})
	go func() {
		h.onModal(session, interaction)
		close(done)
	}()
	select {
	case response := <-capture.responses:
		if response.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
			t.Fatalf("password modal ACK type=%d", response.Type)
		}
	case <-lang.started:
		close(release)
		<-done
		t.Fatal("password modal submit read the language database before ACK")
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("password modal submit was not acknowledged")
	}
	close(release)
	select {
	case edit := <-capture.edits:
		if len(edit.Components) != 1 {
			t.Fatalf("password modal result=%+v", edit)
		}
	case <-time.After(time.Second):
		t.Fatal("password modal submit did not edit its deferred response")
	}
	<-done
}

func TestWishlistSelectionAcknowledgesBeforeWorkAndEditsEphemeralSource(t *testing.T) {
	var acknowledged atomic.Bool
	session, capture := newMFAInteractionCapture(t, &acknowledged)
	wishlist := &acknowledgedWishlistStore{acknowledged: &acknowledged}
	skinStore := &acknowledgedSkinStore{acknowledged: &acknowledged}
	h := &Handlers{Wishlist: wishlist, Skins: skinStore}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-wishlist-select",
		AppID: "application-1",
		Token: "token-wishlist-select",
		Type:  discordgo.InteractionMessageComponent,
		Data: discordgo.MessageComponentInteractionData{
			CustomID: customIDWishlistAddPrefix + "owner-1",
			Values:   []string{"skin-1"},
		},
		User: &discordgo.User{ID: "owner-1"},
	}}

	h.onComponent(session, interaction)
	response := <-capture.responses
	if response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("wishlist ACK type=%d, want deferred source update", response.Type)
	}
	if wishlist.beforeACK.Load() || skinStore.beforeACK.Load() {
		t.Fatal("wishlist persistence or skin lookup ran before component ACK")
	}
	select {
	case edit := <-capture.edits:
		if !strings.Contains(edit.Content, "Prime Vandal") || edit.Components == nil || len(edit.Components) != 0 {
			t.Fatalf("wishlist selection edit=%+v", edit)
		}
	case <-time.After(time.Second):
		t.Fatal("wishlist selection did not edit its ephemeral source")
	}
}

func TestChannelTimeSelectionAcknowledgesEphemerallyBeforeDatabaseWork(t *testing.T) {
	var acknowledged atomic.Bool
	session, capture := newMFAInteractionCapture(t, &acknowledged)
	guilds := &acknowledgedGuildStore{acknowledged: &acknowledged}
	h := &Handlers{Guilds: guilds}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:      "interaction-channel-time",
		AppID:   "application-1",
		Token:   "token-channel-time",
		Type:    discordgo.InteractionMessageComponent,
		GuildID: "guild-1",
		Data: discordgo.MessageComponentInteractionData{
			CustomID: customIDChannelTimePrefix + "guild-1",
			Values:   []string{"21"},
		},
		User: &discordgo.User{ID: "owner-1"},
	}}

	h.onComponent(session, interaction)
	response := <-capture.responses
	if response.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource ||
		response.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Fatalf("channel-time ACK=%+v, want deferred ephemeral response", response)
	}
	if guilds.beforeACK.Load() {
		t.Fatal("channel settings database ran before component ACK")
	}
	select {
	case edit := <-capture.edits:
		if !strings.Contains(edit.Content, "21") || edit.Components == nil || len(edit.Components) != 0 {
			t.Fatalf("channel-time edit=%+v", edit)
		}
	case <-time.After(time.Second):
		t.Fatal("channel-time selection did not edit its deferred ephemeral response")
	}
}

func TestMFAOpenComponentWrongOwnerIsEphemeralAndLocalized(t *testing.T) {
	session, responses := newInteractionResponseCapture(t)
	auth := &mfaInteractionAuth{validateErr: authweb.ErrMFAOwner}
	h := &Handlers{Auth: auth}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-mfa-intruder",
		AppID: "application-1",
		Token: "token-mfa-intruder",
		Type:  discordgo.InteractionMessageComponent,
		Data: discordgo.MessageComponentInteractionData{
			CustomID: customIDAuthMFAOpenPref + "mfa-state-1",
		},
		User: &discordgo.User{ID: "intruder-1"},
	}}

	h.onComponent(session, interaction)
	response := <-responses
	if auth.validateUser != "intruder-1" {
		t.Fatalf("MFA validation user=%q, want intruder-1", auth.validateUser)
	}
	if response.Type != discordgo.InteractionResponseChannelMessageWithSource ||
		response.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Fatalf("wrong-owner MFA response=%+v", response)
	}
	if response.Data.Content != i18n.T(i18n.KO, "auth.mfa.denied") {
		t.Fatalf("wrong-owner content=%q", response.Data.Content)
	}
}

func TestExpiredMFAOpenUpdatesSourceAndClearsStaleControls(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	auth := &mfaInteractionAuth{validateErr: authweb.ErrMFAExpired}
	h := &Handlers{Auth: auth}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-mfa-expired-open",
		AppID: "application-1",
		Token: "token-mfa-expired-open",
		Type:  discordgo.InteractionMessageComponent,
		Data:  discordgo.MessageComponentInteractionData{CustomID: customIDAuthMFAOpenPref + "mfa-state-1"},
		User:  &discordgo.User{ID: "owner-1"},
	}}

	h.onComponent(session, interaction)
	response := <-capture.responses
	if response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("expired MFA open ACK type=%d, want deferred update", response.Type)
	}
	edit := <-capture.edits
	if edit.Content != i18n.T(i18n.KO, "auth.mfa.expired") ||
		edit.Components == nil || len(edit.Components) != 0 {
		t.Fatalf("expired MFA open retained stale controls: %+v", edit)
	}
}

func TestMFAModalSubmitPassesOwnerAndKeepsInvalidCodeRetry(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	auth := &mfaInteractionAuth{completeErr: fmt.Errorf("riot response: %w", riot.ErrPasswordInvalidCode)}
	h := &Handlers{Auth: auth}
	interaction := mfaModalSubmitInteraction("interaction-mfa-submit", "owner-1", "mfa-state-1", "000000")

	h.onModal(session, interaction)
	response := <-capture.responses
	if auth.completeState != "mfa-state-1" || auth.completeUser != "owner-1" || auth.completeCode != "000000" {
		t.Fatalf("MFA submit got state=%q user=%q code=%q", auth.completeState, auth.completeUser, auth.completeCode)
	}
	if auth.validateState != "mfa-state-1" || auth.validateUser != "owner-1" {
		t.Fatalf("MFA prevalidation got state=%q user=%q", auth.validateState, auth.validateUser)
	}
	if response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("invalid-code MFA ACK type=%d, want deferred update", response.Type)
	}
	edit := <-capture.edits
	if edit.Content != i18n.T(i18n.KO, "auth.mfa.invalid") || len(edit.Components) != 1 ||
		len(edit.Components[0].Components) != 1 || edit.Components[0].Components[0].CustomID != customIDAuthMFAOpenPref+"mfa-state-1" {
		t.Fatalf("invalid-code MFA edit=%+v", edit)
	}
}

func TestMFAModalSubmitValidatesBeforeLanguageStoreRead(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	auth := &mfaInteractionAuth{completeErr: fmt.Errorf("riot response: %w", riot.ErrPasswordInvalidCode)}
	lang := &mfaLanguageOrderStore{auth: auth}
	h := &Handlers{Auth: auth, Lang: lang}

	h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-order", "owner-1", "mfa-state-1", "000000"))
	<-capture.responses
	<-capture.edits
	if lang.readBeforeValidation.Load() {
		t.Fatal("language store was read before in-memory MFA validation")
	}
}

func TestMFAModalSubmitAcknowledgesBeforeBlockingCompletion(t *testing.T) {
	var acknowledged atomic.Bool
	session, capture := newMFAInteractionCapture(t, &acknowledged)
	completeStarted := make(chan struct{}, 1)
	completeRelease := make(chan struct{})
	auth := &mfaInteractionAuth{
		acknowledged:         &acknowledged,
		firstCompleteStarted: completeStarted,
		firstCompleteRelease: completeRelease,
		completeErr:          fmt.Errorf("riot response: %w", riot.ErrPasswordInvalidCode),
	}
	h := &Handlers{Auth: auth}
	done := make(chan struct{})
	go func() {
		h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-blocking", "owner-1", "mfa-state-1", "000000"))
		close(done)
	}()

	select {
	case <-completeStarted:
	case <-time.After(time.Second):
		close(completeRelease)
		t.Fatal("MFA completion did not start")
	}
	ackedBeforeRelease := acknowledged.Load()
	completeBeforeACK := auth.completeBeforeACK.Load()
	close(completeRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("MFA modal handler did not finish")
	}
	response := <-capture.responses
	if !ackedBeforeRelease || completeBeforeACK {
		t.Fatalf("MFA completion started before ACK: acknowledged=%v beforeACK=%v", ackedBeforeRelease, completeBeforeACK)
	}
	if response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("MFA ACK type=%d, want deferred update", response.Type)
	}
	select {
	case <-capture.edits:
	case <-time.After(time.Second):
		t.Fatal("invalid MFA result did not edit the originating message")
	}
}

// Mutation caught: holding mfaSubmissionGuard's mutex across Riot completion
// couples the edit-control lock to external I/O and can block unrelated cleanup.
func TestMFASubmitDoesNotHoldGuardMutexAcrossRiotIO(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	completeStarted := make(chan struct{}, 1)
	completeRelease := make(chan struct{})
	auth := &mfaInteractionAuth{
		firstCompleteStarted: completeStarted,
		firstCompleteRelease: completeRelease,
		completeErr:          fmt.Errorf("riot response: %w", riot.ErrPasswordInvalidCode),
	}
	h := &Handlers{Auth: auth}
	done := make(chan struct{})
	go func() {
		h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-guard-io", "owner-1", "mfa-state-1", "000000"))
		close(done)
	}()
	select {
	case <-completeStarted:
	case <-time.After(time.Second):
		close(completeRelease)
		t.Fatal("MFA completion did not start")
	}
	guard := h.mfaSubmissionGuard("mfa-state-1")
	if !guard.TryLock() {
		close(completeRelease)
		<-done
		t.Fatal("MFA submission guard mutex was held across Riot I/O")
	}
	guard.Unlock()
	close(completeRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("MFA submit did not finish")
	}
	<-capture.responses
	<-capture.edits
}

func TestMFAModalWrongOwnerDoesNotEditOwnerMessage(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	auth := &mfaInteractionAuth{validateErr: authweb.ErrMFAOwner}
	h := &Handlers{Auth: auth}
	h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-wrong-owner", "intruder-1", "mfa-state-1", "123456"))

	response := <-capture.responses
	if response.Type != discordgo.InteractionResponseChannelMessageWithSource ||
		response.Data.Flags&discordgo.MessageFlagsEphemeral == 0 || response.Data.Content != i18n.T(i18n.KO, "auth.mfa.denied") {
		t.Fatalf("wrong-owner MFA response=%+v", response)
	}
	select {
	case edit := <-capture.edits:
		t.Fatalf("wrong-owner MFA edited owner message: %+v", edit)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestExpiredMFAModalUpdatesSourceAndClearsStaleControls(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	auth := &mfaInteractionAuth{validateErr: authweb.ErrMFAExpired}
	h := &Handlers{Auth: auth}
	h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-expired", "owner-1", "mfa-state-1", "123456"))

	response := <-capture.responses
	if response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("expired MFA ACK type=%d, want deferred update", response.Type)
	}
	edit := <-capture.edits
	if edit.Content != i18n.T(i18n.KO, "auth.mfa.expired") || edit.Components == nil || len(edit.Components) != 0 {
		t.Fatalf("expired MFA edit retained stale controls: %+v", edit)
	}
	if calls := auth.completeCalls.Load(); calls != 0 {
		t.Fatalf("expired MFA reached completion %d time(s)", calls)
	}
}

func TestMFAModalSubmitSuccessAndTerminalFailureClearOriginatingComponents(t *testing.T) {
	tests := []struct {
		name    string
		display string
		err     error
	}{
		{name: "success", display: "Player#KR1"},
		{name: "terminal", err: errors.New("sqlite write failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session, capture := newMFAInteractionCapture(t, nil)
			auth := &mfaInteractionAuth{completeDisplay: tt.display, completeErr: tt.err}
			h := &Handlers{Auth: auth}
			h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-"+tt.name, "owner-1", "mfa-state-1", "123456"))

			response := <-capture.responses
			if response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
				t.Fatalf("MFA ACK type=%d, want deferred update", response.Type)
			}
			edit := <-capture.edits
			if edit.Components == nil || len(edit.Components) != 0 {
				t.Fatalf("terminal MFA edit retained components: %+v", edit)
			}
		})
	}
}

func TestConcurrentMFAModalResultsDoNotOverwriteSuccess(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	firstStarted := make(chan struct{}, 1)
	firstRelease := make(chan struct{})
	auth := &mfaInteractionAuth{
		firstCompleteStarted: firstStarted,
		firstCompleteRelease: firstRelease,
		completeResults: []mfaCompletionResult{
			{display: "Player#KR1"},
			{err: authweb.ErrMFAExpired},
		},
	}
	h := &Handlers{Auth: auth}
	firstDone := make(chan struct{})
	go func() {
		h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-first", "owner-1", "mfa-state-1", "123456"))
		close(firstDone)
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		close(firstRelease)
		t.Fatal("first MFA completion did not start")
	}
	secondDone := make(chan struct{})
	go func() {
		h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-second", "owner-1", "mfa-state-1", "654321"))
		close(secondDone)
	}()

	callbacksBeforeRelease := 0
	deadline := time.After(200 * time.Millisecond)
collectCallbacks:
	for callbacksBeforeRelease < 2 {
		select {
		case <-capture.responses:
			callbacksBeforeRelease++
		case <-deadline:
			break collectCallbacks
		}
	}
	close(firstRelease)
	for _, done := range []<-chan struct{}{firstDone, secondDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent MFA handler did not finish")
		}
	}
	if callbacksBeforeRelease != 2 {
		t.Fatalf("MFA ACKs before first completion=%d, want 2", callbacksBeforeRelease)
	}
	if calls := auth.completeCalls.Load(); calls != 1 {
		t.Fatalf("concurrent MFA completion calls=%d, want 1", calls)
	}
	var edits []capturedInteractionEdit
	drain := time.After(100 * time.Millisecond)
drainEdits:
	for {
		select {
		case edit := <-capture.edits:
			edits = append(edits, edit)
		case <-drain:
			break drainEdits
		}
	}
	if len(edits) != 1 || !strings.Contains(edits[0].Content, "Player#KR1") || len(edits[0].Components) != 0 {
		t.Fatalf("concurrent MFA edits=%+v, want one component-free success", edits)
	}
}

func TestLateAlreadyOpenMFAModalDoesNotOverwriteTerminalSuccess(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	auth := &consumedAfterSuccessMFAAuth{mfaInteractionAuth: mfaInteractionAuth{completeDisplay: "Player#KR1"}}
	h := &Handlers{Auth: auth}

	// Both modals could have been opened while the continuation was live. The
	// first submit consumes it and publishes success; the late second submit
	// then validates as expired and must not rewrite that terminal source.
	h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-success", "owner-1", "mfa-state-1", "123456"))
	if response := <-capture.responses; response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("successful MFA ACK type=%d, want deferred update", response.Type)
	}
	firstEdit := <-capture.edits
	if !strings.Contains(firstEdit.Content, "Player#KR1") || len(firstEdit.Components) != 0 {
		t.Fatalf("successful MFA edit=%+v", firstEdit)
	}

	h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-late", "owner-1", "mfa-state-1", "654321"))
	if response := <-capture.responses; response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("late MFA ACK type=%d, want deferred update", response.Type)
	}
	select {
	case edit := <-capture.edits:
		t.Fatalf("late expired MFA overwrote terminal success: %+v", edit)
	case <-time.After(100 * time.Millisecond):
	}
	if calls := auth.completeCalls.Load(); calls != 1 {
		t.Fatalf("late MFA completion calls=%d, want 1", calls)
	}
}

func TestLateMFAOpenDoesNotOverwriteTerminalSuccess(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	auth := &consumedAfterSuccessMFAAuth{mfaInteractionAuth: mfaInteractionAuth{completeDisplay: "Player#KR1"}}
	h := &Handlers{Auth: auth}

	h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-success", "owner-1", "mfa-state-1", "123456"))
	if response := <-capture.responses; response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("successful MFA ACK type=%d, want deferred update", response.Type)
	}
	firstEdit := <-capture.edits
	if !strings.Contains(firstEdit.Content, "Player#KR1") || len(firstEdit.Components) != 0 {
		t.Fatalf("successful MFA edit=%+v", firstEdit)
	}

	lateOpen := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-mfa-late-open",
		AppID: "application-1",
		Token: "token-mfa-late-open",
		Type:  discordgo.InteractionMessageComponent,
		Data: discordgo.MessageComponentInteractionData{
			CustomID: customIDAuthMFAOpenPref + "mfa-state-1",
		},
		User: &discordgo.User{ID: "owner-1"},
	}}
	h.onComponent(session, lateOpen)
	if response := <-capture.responses; response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("late MFA open ACK type=%d, want deferred update", response.Type)
	}
	select {
	case edit := <-capture.edits:
		t.Fatalf("late expired MFA open overwrote terminal success: %+v", edit)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestInteractionLogCustomIDRedactsAuthContinuationState(t *testing.T) {
	tests := []struct {
		customID string
		want     string
	}{
		{customID: customIDAuthCaptchaPref + "captcha-secret-state", want: "auth:captcha"},
		{customID: customIDAuthMFAOpenPref + "mfa-secret-state", want: "auth:mfaopen"},
		{customID: customIDAuthMFAPref + "mfa-secret-state", want: "auth:mfa"},
		{customID: customIDAuthPassword, want: customIDAuthPassword},
		{customID: "shop:page:owner:1", want: "shop:page:owner:1"},
	}
	for _, tt := range tests {
		if got := interactionLogCustomID(tt.customID); got != tt.want {
			t.Errorf("interactionLogCustomID(%q)=%q, want %q", tt.customID, got, tt.want)
		}
	}
}

func TestContinuationLogValueNeverReturnsAuthState(t *testing.T) {
	for _, state := range []string{"", "qr-secret-state", "password-secret-state", strings.Repeat("s", 512)} {
		got := continuationLogValue(state)
		if got != "<redacted>" || (state != "" && strings.Contains(got, state)) {
			t.Fatalf("continuationLogValue leaked input: got=%q", got)
		}
	}
}

func TestHandlersShutdownCancelsAndJoinsQRAndPasswordWatchers(t *testing.T) {
	session, _ := newMFAInteractionCapture(t, nil)
	auth := &cancelingWatcherAuth{
		qrStarted:       make(chan struct{}),
		passwordStarted: make(chan struct{}),
	}
	h := &Handlers{Auth: auth}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-watcher-shutdown",
		AppID: "application-1",
		Token: "token-watcher-shutdown",
		User:  &discordgo.User{ID: "owner-1"},
	}}
	h.startQRLoginWatcher(session, interaction, "qr-secret-state", i18n.KO)
	h.startPasswordCaptchaWatcher(session, interaction, "password-secret-state", i18n.KO)
	for _, started := range []<-chan struct{}{auth.qrStarted, auth.passwordStarted} {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("auth watcher did not start")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	h.captchaWatchMu.Lock()
	watches := len(h.captchaWatches)
	h.captchaWatchMu.Unlock()
	if watches != 0 {
		t.Fatalf("shutdown retained %d CAPTCHA watcher(s)", watches)
	}
}

func TestHandlersShutdownCancelsJoinsAndSealsInteractionCallbacks(t *testing.T) {
	session, _ := newMFAInteractionCapture(t, nil)
	release := make(chan struct{})
	auth := &lifecycleInteractionAuth{started: make(chan struct{}, 1), release: release}
	h := &Handlers{Auth: auth}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-lifecycle-password",
		AppID: "application-1",
		Token: "token-lifecycle-password",
		Type:  discordgo.InteractionModalSubmit,
		Data: discordgo.ModalSubmitInteractionData{
			CustomID: customIDAuthPWModal,
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.TextInput{CustomID: "username", Value: "user"},
				discordgo.TextInput{CustomID: "password", Value: "pass"},
			}}},
		},
		User: &discordgo.User{ID: "owner-1"},
	}}
	interactionDone := make(chan struct{})
	go func() {
		h.OnInteraction(session, interaction)
		close(interactionDone)
	}()
	select {
	case <-auth.started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("password interaction did not reach auth work")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		close(release)
		t.Fatal(err)
	}
	if !auth.canceled.Load() {
		close(release)
		select {
		case <-interactionDone:
		case <-time.After(time.Second):
		}
		t.Fatal("handler shutdown returned before canceling the in-flight interaction")
	}
	select {
	case <-interactionDone:
	case <-time.After(time.Second):
		t.Fatal("handler shutdown did not join the in-flight interaction")
	}

	// Once shutdown seals the handler, a newly delivered callback must not
	// begin browser/database/auth work.
	h.OnInteraction(session, interaction)
	if calls := auth.calls.Load(); calls != 1 {
		t.Fatalf("post-shutdown interaction reached auth %d times, want 1", calls)
	}
}

func TestMFATerminalFailureClearsCachedHint(t *testing.T) {
	auth := &mfaInteractionAuth{completeErr: errors.New("identity lookup failed")}
	h := &Handlers{Auth: auth}
	h.rememberMFAHint("mfa-state-1", "a***@example.com")

	response, err := h.HandlePasswordMFA(context.Background(), "mfa-state-1", "owner-1", "123456", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.mfaHintFor("mfa-state-1"); got != "" {
		t.Fatalf("terminal MFA failure retained cached hint %q", got)
	}
	if len(response.Components) != 0 {
		t.Fatalf("terminal MFA failure exposed retry controls: %#v", response.Components)
	}
}

// Mutation caught: omitting owner-bound rollback after a failed terminal edit
// retains an unreachable MFA continuation; canceling on success, no MFA, or an
// owner mismatch destroys a continuation that may still be reachable elsewhere.
func TestPasswordCaptchaWatcherRollsBackOnlyUndeliveredOwnedMFA(t *testing.T) {
	tests := []struct {
		name              string
		failedEdit        bool
		mfaState          string
		cancelErr         error
		wantCancelCalls   int32
		wantCancelSuccess bool
		wantLocalState    bool
	}{
		{
			name:              "failed terminal edit cancels owner continuation",
			failedEdit:        true,
			mfaState:          "mfa-state-1",
			wantCancelCalls:   1,
			wantCancelSuccess: true,
		},
		{
			name:           "successful terminal edit keeps continuation",
			mfaState:       "mfa-state-1",
			wantLocalState: true,
		},
		{
			name:       "failed edit without MFA has nothing to cancel",
			failedEdit: true,
		},
		{
			name:            "wrong owner cannot cancel continuation",
			failedEdit:      true,
			mfaState:        "mfa-state-1",
			cancelErr:       authweb.ErrMFAOwner,
			wantCancelCalls: 1,
			wantLocalState:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := &mfaDeliveryAuth{
				waitMFAState: test.mfaState,
				waitMFAHint:  "a***@example.com",
				cancelErr:    test.cancelErr,
			}
			var session *discordgo.Session
			if test.failedEdit {
				session = newFailedOriginalEditSession(t)
			} else {
				session, _ = newMFAInteractionCapture(t, nil)
			}
			h := &Handlers{Auth: auth}
			if test.mfaState != "" {
				h.mfaSubmissionGuard(test.mfaState)
			}
			t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
			interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
				ID: "interaction-mfa-delivery", AppID: "application-1", Token: "token-mfa-delivery",
				User: &discordgo.User{ID: "owner-1"},
			}}

			h.watchPasswordCaptcha(context.Background(), session, interaction, "captcha-state", i18n.KO)

			if got := auth.cancelCalls.Load(); got != test.wantCancelCalls {
				t.Errorf("MFA cancellation calls=%d, want %d", got, test.wantCancelCalls)
			}
			if got := auth.cancelSucceeded.Load(); got != test.wantCancelSuccess {
				t.Errorf("MFA cancellation success=%v, want %v", got, test.wantCancelSuccess)
			}
			auth.cancelMu.Lock()
			cancelState, cancelUser := auth.cancelState, auth.cancelUser
			auth.cancelMu.Unlock()
			if test.wantCancelCalls != 0 && (cancelState != test.mfaState || cancelUser != "owner-1") {
				t.Errorf("MFA cancellation state=%q user=%q", cancelState, cancelUser)
			}
			localState := h.mfaHintFor(test.mfaState) != ""
			h.mfaSubmitMu.Lock()
			_, guardExists := h.mfaSubmitGuards[test.mfaState]
			h.mfaSubmitMu.Unlock()
			localState = localState || guardExists
			if localState != test.wantLocalState {
				t.Errorf("cached MFA hint/control retained=%v, want %v", localState, test.wantLocalState)
			}
		})
	}
}

func TestAuthQRComponent_AcknowledgesThenEditsWithMappedAttachment(t *testing.T) {
	type attachment struct {
		ID       string `json:"id"`
		Filename string `json:"filename"`
	}
	type editPayload struct {
		Attachments []attachment              `json:"attachments"`
		Components  []map[string]any          `json:"components"`
		Embeds      []*discordgo.MessageEmbed `json:"embeds"`
	}

	var (
		acknowledged atomic.Bool
		callbackType atomic.Int64
		gotEdit      editPayload
		gotFileName  string
		gotFileBody  []byte
		editCalls    atomic.Int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/callback":
			var response discordgo.InteractionResponse
			if err := json.NewDecoder(r.Body).Decode(&response); err != nil {
				t.Errorf("decode callback: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			callbackType.Store(int64(response.Type))
			if response.Type == discordgo.InteractionResponseDeferredMessageUpdate {
				acknowledged.Store(true)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/original":
			editCalls.Add(1)
			reader, err := r.MultipartReader()
			if err != nil {
				t.Errorf("MultipartReader: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("NextPart: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				body, err := io.ReadAll(part)
				if err != nil {
					t.Errorf("ReadAll(%s): %v", part.FormName(), err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch part.FormName() {
				case "payload_json":
					if err := json.Unmarshal(body, &gotEdit); err != nil {
						t.Errorf("payload_json: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				case "files[0]":
					gotFileName = part.FileName()
					gotFileBody = body
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	originalCallbackEndpoint := discordgo.EndpointInteractionResponse
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return srv.URL + "/original" }
	t.Cleanup(func() {
		discordgo.EndpointInteractionResponse = originalCallbackEndpoint
		discordgo.EndpointWebhookMessage = originalWebhookEndpoint
	})

	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	auth := &acknowledgementCheckingAuth{acknowledged: &acknowledged}
	lang := &ackLanguageStore{acknowledged: &acknowledged}
	h := &Handlers{Auth: auth, Lang: lang}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-1",
		AppID: "application-1",
		Token: "token-1",
		Type:  discordgo.InteractionMessageComponent,
		Data: discordgo.MessageComponentInteractionData{
			CustomID: customIDAuthQR,
		},
		User: &discordgo.User{ID: "discord-1"},
	}}

	h.onComponent(session, interaction)

	if auth.beginBeforeACK.Load() {
		t.Error("BeginQRAuth ran before Discord acknowledged the component interaction")
	}
	if lang.readBeforeACK.Load() {
		t.Error("language database was read before Discord acknowledged the QR component interaction")
	}
	if got := discordgo.InteractionResponseType(callbackType.Load()); got != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("callback type = %d, want deferred message update (%d)", got, discordgo.InteractionResponseDeferredMessageUpdate)
	}
	if editCalls.Load() != 1 {
		t.Fatalf("original-message edit calls = %d, want 1", editCalls.Load())
	}
	if len(gotEdit.Attachments) != 1 {
		t.Fatalf("attachments = %#v, want one multipart attachment mapping", gotEdit.Attachments)
	}
	if got := gotEdit.Attachments[0]; got.ID != "0" || got.Filename != "riot-qr.png" {
		t.Fatalf("attachment = %#v, want id=0 filename=riot-qr.png", got)
	}
	if gotFileName != "riot-qr.png" || len(gotFileBody) == 0 {
		t.Fatalf("files[0] = (%q, %d bytes), want non-empty riot-qr.png", gotFileName, len(gotFileBody))
	}
	if len(gotEdit.Embeds) != 1 || gotEdit.Embeds[0].Image == nil || gotEdit.Embeds[0].Image.URL != "attachment://riot-qr.png" {
		t.Fatalf("QR embed = %#v", gotEdit.Embeds)
	}
	if len(gotEdit.Components) != 1 {
		t.Fatalf("components = %#v, want Riot Mobile link button", gotEdit.Components)
	}
	if _, format, err := image.Decode(bytes.NewReader(gotFileBody)); err != nil || format != "png" {
		t.Fatalf("QR upload is not a decodable PNG: format=%q err=%v", format, err)
	}
}

func TestAuthCaptchaComponentAcknowledgesBeforeLaunchingChrome(t *testing.T) {
	var (
		acknowledged atomic.Bool
		callbackType atomic.Int64
		editCalls    atomic.Int64
	)
	launchStarted := make(chan struct{}, 1)
	launchRelease := make(chan struct{})
	waitStarted := make(chan struct{}, 1)
	waitRelease := make(chan struct{})
	waitDone := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/callback":
			var response discordgo.InteractionResponse
			if err := json.NewDecoder(r.Body).Decode(&response); err != nil {
				t.Errorf("decode callback: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			callbackType.Store(int64(response.Type))
			if response.Type == discordgo.InteractionResponseDeferredMessageUpdate {
				acknowledged.Store(true)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/original":
			editCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	originalCallbackEndpoint := discordgo.EndpointInteractionResponse
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return srv.URL + "/original" }
	t.Cleanup(func() {
		discordgo.EndpointInteractionResponse = originalCallbackEndpoint
		discordgo.EndpointWebhookMessage = originalWebhookEndpoint
	})

	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	auth := &acknowledgementCheckingAuth{
		acknowledged:  &acknowledged,
		launchStarted: launchStarted,
		launchRelease: launchRelease,
		waitStarted:   waitStarted,
		waitRelease:   waitRelease,
		waitDone:      waitDone,
	}
	h := &Handlers{Auth: auth}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-captcha",
		AppID: "application-1",
		Token: "token-captcha",
		Type:  discordgo.InteractionMessageComponent,
		Data: discordgo.MessageComponentInteractionData{
			CustomID: customIDAuthCaptchaPref + "state-1",
		},
		User: &discordgo.User{ID: "discord-1"},
	}}

	done := make(chan struct{})
	go func() {
		h.onComponent(session, interaction)
		close(done)
	}()
	select {
	case <-launchStarted:
	case <-time.After(time.Second):
		close(launchRelease)
		t.Fatal("LaunchPasswordCaptcha did not start")
	}
	ackedBeforeLaunchCompleted := acknowledged.Load()
	close(launchRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("captcha component handler did not finish")
	}

	if auth.launchBeforeACK.Load() || !ackedBeforeLaunchCompleted {
		t.Error("LaunchPasswordCaptcha ran before Discord acknowledged the component interaction")
	}
	if got := discordgo.InteractionResponseType(callbackType.Load()); got != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("callback type = %d, want deferred message update (%d)", got, discordgo.InteractionResponseDeferredMessageUpdate)
	}
	if editCalls.Load() != 1 {
		t.Fatalf("original-message edit calls = %d, want 1", editCalls.Load())
	}
	select {
	case <-waitStarted:
	case <-time.After(time.Second):
		t.Fatal("successful captcha launch did not start its watcher")
	}
	close(waitRelease)
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("captcha watcher did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for editCalls.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if editCalls.Load() != 2 {
		t.Fatalf("original-message edit calls = %d, want 2 including watcher completion", editCalls.Load())
	}
}

func TestPasswordModalStartsOneWatcherAfterOwnerCaptchaButton(t *testing.T) {
	type component struct {
		CustomID   string      `json:"custom_id"`
		Components []component `json:"components"`
	}
	type modalResponsePayload struct {
		Type discordgo.InteractionResponseType `json:"type"`
		Data struct {
			Components []component `json:"components"`
		} `json:"data"`
	}
	type modalEditPayload struct {
		Components []component `json:"components"`
	}
	modalACK := make(chan modalResponsePayload, 1)
	modalEdit := make(chan modalEditPayload, 1)
	originalEdits := make(chan struct{}, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/callback":
			var response modalResponsePayload
			if err := json.NewDecoder(r.Body).Decode(&response); err != nil {
				t.Errorf("decode callback: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if response.Type == discordgo.InteractionResponseDeferredChannelMessageWithSource {
				modalACK <- response
			}
			w.WriteHeader(http.StatusNoContent)
		case "/original":
			var edit modalEditPayload
			if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
				t.Errorf("decode edit: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			select {
			case modalEdit <- edit:
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
			originalEdits <- struct{}{}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	originalCallbackEndpoint := discordgo.EndpointInteractionResponse
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return srv.URL + "/original" }
	t.Cleanup(func() {
		discordgo.EndpointInteractionResponse = originalCallbackEndpoint
		discordgo.EndpointWebhookMessage = originalWebhookEndpoint
	})

	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	waitRelease := make(chan struct{})
	waitStarted := make(chan struct{}, 2)
	waitDone := make(chan struct{}, 1)
	auth := &passwordButtonAuth{waitStarted: waitStarted, waitRelease: waitRelease, waitDone: waitDone}
	h := &Handlers{Auth: auth}
	modal := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-password-modal",
		AppID: "application-1",
		Token: "token-password-modal",
		Type:  discordgo.InteractionModalSubmit,
		Data: discordgo.ModalSubmitInteractionData{
			CustomID: customIDAuthPWModal,
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.TextInput{CustomID: "username", Value: "user"},
				discordgo.TextInput{CustomID: "password", Value: "pass"},
			}}},
		},
		User: &discordgo.User{ID: "owner-1"},
	}}
	h.onModal(session, modal)
	if response := <-modalACK; response.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
		t.Fatalf("password modal ACK type=%d", response.Type)
	}

	select {
	case response := <-modalEdit:
		if len(response.Components) != 1 {
			t.Fatalf("modal response components = %#v", response.Components)
		}
		row := response.Components[0]
		if len(row.Components) != 1 {
			t.Fatalf("modal response row = %#v", response.Components[0])
		}
		button := row.Components[0]
		if button.CustomID != customIDAuthCaptchaPref+"captcha-state" {
			t.Fatalf("captcha button = %#v", row.Components[0])
		}
	case <-time.After(time.Second):
		t.Fatal("password modal did not return its captcha button")
	}
	<-originalEdits // password modal's deferred-response edit
	if got := auth.launches.Load(); got != 0 {
		t.Fatalf("modal Chrome launches = %d, want 0", got)
	}
	select {
	case <-waitStarted:
		t.Error("password modal started its captcha watcher before the button click")
	case <-time.After(100 * time.Millisecond):
	}

	captchaComponent := func(id string) *discordgo.InteractionCreate {
		return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
			ID:    id,
			AppID: "application-1",
			Token: id + "-token",
			Type:  discordgo.InteractionMessageComponent,
			Data:  discordgo.MessageComponentInteractionData{CustomID: customIDAuthCaptchaPref + "captcha-state"},
			User:  &discordgo.User{ID: "owner-1"},
		}}
	}
	h.onComponent(session, captchaComponent("interaction-captcha-button"))
	if got := auth.launches.Load(); got != 1 {
		t.Fatalf("owner button Chrome launches = %d, want 1", got)
	}
	select {
	case <-originalEdits:
	case <-time.After(time.Second):
		t.Fatal("captcha button response did not finish editing")
	}
	select {
	case <-waitStarted:
	case <-time.After(time.Second):
		t.Fatal("first successful captcha launch did not start its watcher")
	}

	h.onComponent(session, captchaComponent("interaction-captcha-reopen"))
	if got := auth.launches.Load(); got != 2 {
		t.Fatalf("reopen Chrome launches = %d, want 2", got)
	}
	select {
	case <-originalEdits:
	case <-time.After(time.Second):
		t.Fatal("captcha reopen response did not finish editing")
	}
	select {
	case <-waitStarted:
		t.Error("captcha reopen started a second watcher")
	case <-time.After(100 * time.Millisecond):
	}
	if got := auth.waits.Load(); got != 1 {
		t.Fatalf("captcha watchers = %d, want 1", got)
	}
	close(waitRelease)
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("captcha watcher did not finish")
	}
	select {
	case <-originalEdits:
	case <-time.After(time.Second):
		t.Fatal("captcha watcher did not finish editing")
	}
}

func TestPasswordModalEditFailureCancelsUndeliveredPasswordState(t *testing.T) {
	session := newFailedOriginalEditSession(t)
	auth := &passwordButtonAuth{}
	h := &Handlers{Auth: auth}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-password-edit-failure",
		AppID: "application-1",
		Token: "token-password-edit-failure",
		Type:  discordgo.InteractionModalSubmit,
		Data: discordgo.ModalSubmitInteractionData{
			CustomID: customIDAuthPWModal,
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.TextInput{CustomID: "username", Value: "user"},
				discordgo.TextInput{CustomID: "password", Value: "pass"},
			}}},
		},
		User: &discordgo.User{ID: "owner-1"},
	}}

	h.onModal(session, interaction)
	if got := auth.cancelCalls.Load(); got != 1 || auth.cancelState != "captcha-state" || auth.cancelUser != "owner-1" {
		t.Fatalf("undelivered password-state cancellations=%d state=%q user=%q", got, auth.cancelState, auth.cancelUser)
	}
}

func TestCaptchaLaunchEditFailureCancelsLaunchedPasswordState(t *testing.T) {
	session := newFailedOriginalEditSession(t)
	auth := &passwordButtonAuth{}
	h := &Handlers{Auth: auth}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-captcha-edit-failure",
		AppID: "application-1",
		Token: "token-captcha-edit-failure",
		Type:  discordgo.InteractionMessageComponent,
		Data: discordgo.MessageComponentInteractionData{
			CustomID: customIDAuthCaptchaPref + "captcha-state",
		},
		User: &discordgo.User{ID: "owner-1"},
	}}

	h.onComponent(session, interaction)
	if got := auth.launches.Load(); got != 1 {
		t.Fatalf("captcha launches=%d, want 1", got)
	}
	if got := auth.cancelCalls.Load(); got != 1 || auth.cancelState != "captcha-state" || auth.cancelUser != "owner-1" {
		t.Fatalf("orphaned launch cancellations=%d state=%q user=%q", got, auth.cancelState, auth.cancelUser)
	}
	if got := auth.waits.Load(); got != 0 {
		t.Fatalf("failed launch-status edit started %d watcher(s)", got)
	}
}

func TestTerminalCaptchaLaunchErrorCancelsPasswordState(t *testing.T) {
	auth := &passwordButtonAuth{launchErr: errors.New("Chrome failed to start")}
	h := &Handlers{Auth: auth}
	resp, launched, err := h.handlePasswordCaptchaLaunch(context.Background(), "captcha-state", "owner-1", i18n.KO)
	if err != nil || launched || resp.Components == nil || len(resp.Components) != 0 {
		t.Fatalf("terminal launch response=%+v launched=%v err=%v", resp, launched, err)
	}
	if got := auth.cancelCalls.Load(); got != 1 || auth.cancelState != "captcha-state" || auth.cancelUser != "owner-1" {
		t.Fatalf("terminal launch cancellations=%d state=%q user=%q", got, auth.cancelState, auth.cancelUser)
	}
}

func TestPasswordCaptchaLaunchFailureDoesNotStartWatcher(t *testing.T) {
	originalEdits := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/callback":
			w.WriteHeader(http.StatusNoContent)
		case "/original":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
			originalEdits <- struct{}{}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	originalCallbackEndpoint := discordgo.EndpointInteractionResponse
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return srv.URL + "/original" }
	t.Cleanup(func() {
		discordgo.EndpointInteractionResponse = originalCallbackEndpoint
		discordgo.EndpointWebhookMessage = originalWebhookEndpoint
	})

	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	waitRelease := make(chan struct{})
	waitStarted := make(chan struct{}, 1)
	waitDone := make(chan struct{}, 1)
	auth := &passwordButtonAuth{
		launchErr:   errors.New("Chrome/Chromium not found"),
		waitStarted: waitStarted,
		waitRelease: waitRelease,
		waitDone:    waitDone,
	}
	h := &Handlers{Auth: auth}
	modal := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-password-failed-modal",
		AppID: "application-1",
		Token: "token-password-failed-modal",
		Type:  discordgo.InteractionModalSubmit,
		Data: discordgo.ModalSubmitInteractionData{
			CustomID: customIDAuthPWModal,
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.TextInput{CustomID: "username", Value: "user"},
				discordgo.TextInput{CustomID: "password", Value: "pass"},
			}}},
		},
		User: &discordgo.User{ID: "owner-1"},
	}}
	h.onModal(session, modal)
	h.onComponent(session, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-captcha-failed-button",
		AppID: "application-1",
		Token: "token-captcha-failed-button",
		Type:  discordgo.InteractionMessageComponent,
		Data:  discordgo.MessageComponentInteractionData{CustomID: customIDAuthCaptchaPref + "captcha-state"},
		User:  &discordgo.User{ID: "owner-1"},
	}})
	select {
	case <-originalEdits:
	case <-time.After(time.Second):
		t.Fatal("failed captcha launch did not return its component response")
	}
	select {
	case <-waitStarted:
		close(waitRelease)
		select {
		case <-waitDone:
		case <-time.After(time.Second):
			t.Fatal("unexpected captcha watcher did not finish")
		}
		select {
		case <-originalEdits:
		case <-time.After(time.Second):
			t.Fatal("unexpected captcha watcher did not finish editing")
		}
		t.Fatal("failed captcha launch started a watcher")
	case <-time.After(100 * time.Millisecond):
	}
	if got := auth.waits.Load(); got != 0 {
		t.Fatalf("failed captcha launch watchers = %d, want 0", got)
	}
	close(waitRelease)
}
