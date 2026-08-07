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
	launchErr   error
	launches    atomic.Int32
	waits       atomic.Int32
	waitStarted chan<- struct{}
	waitRelease <-chan struct{}
	waitDone    chan<- struct{}
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

func (a *passwordButtonAuth) WaitPasswordLogin(context.Context, string) (string, string, string, error) {
	a.waits.Add(1)
	if a.waitStarted != nil {
		a.waitStarted <- struct{}{}
	}
	<-a.waitRelease
	if a.waitDone != nil {
		a.waitDone <- struct{}{}
	}
	return "", "", "", context.Canceled
}

func (*passwordButtonAuth) ValidatePasswordMFA(string, string) (string, error) {
	return "", nil
}

func (*passwordButtonAuth) CompletePasswordMFA(context.Context, string, string, string) (string, error) {
	return "", nil
}

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

func TestMFAModalSubmitRejectedBeforeACKDoesNotEditOwnerMessage(t *testing.T) {
	tests := []struct {
		name    string
		userID  string
		err     error
		message string
	}{
		{name: "wrong owner", userID: "intruder-1", err: authweb.ErrMFAOwner, message: i18n.T(i18n.KO, "auth.mfa.denied")},
		{name: "expired", userID: "owner-1", err: authweb.ErrMFAExpired, message: i18n.T(i18n.KO, "auth.mfa.expired")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session, capture := newMFAInteractionCapture(t, nil)
			auth := &mfaInteractionAuth{validateErr: tt.err}
			h := &Handlers{Auth: auth}
			h.onModal(session, mfaModalSubmitInteraction("interaction-mfa-rejected", tt.userID, "mfa-state-1", "123456"))

			response := <-capture.responses
			if response.Type != discordgo.InteractionResponseChannelMessageWithSource ||
				response.Data.Flags&discordgo.MessageFlagsEphemeral == 0 || response.Data.Content != tt.message {
				t.Fatalf("rejected MFA response=%+v", response)
			}
			if calls := auth.completeCalls.Load(); calls != 0 {
				t.Fatalf("rejected MFA reached completion %d time(s)", calls)
			}
			select {
			case edit := <-capture.edits:
				t.Fatalf("rejected MFA edited owner message: %+v", edit)
			case <-time.After(50 * time.Millisecond):
			}
		})
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
	h := &Handlers{Auth: auth}
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
	modalResponse := make(chan modalResponsePayload, 1)
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
			if response.Type == discordgo.InteractionResponseChannelMessageWithSource {
				modalResponse <- response
			}
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

	select {
	case response := <-modalResponse:
		if len(response.Data.Components) != 1 {
			t.Fatalf("modal response components = %#v", response.Data.Components)
		}
		row := response.Data.Components[0]
		if len(row.Components) != 1 {
			t.Fatalf("modal response row = %#v", response.Data.Components[0])
		}
		button := row.Components[0]
		if button.CustomID != customIDAuthCaptchaPref+"captcha-state" {
			t.Fatalf("captcha button = %#v", row.Components[0])
		}
	case <-time.After(time.Second):
		t.Fatal("password modal did not return its captcha button")
	}
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
