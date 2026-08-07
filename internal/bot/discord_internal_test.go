package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type acknowledgementCheckingAuth struct {
	acknowledged    *atomic.Bool
	beginBeforeACK  atomic.Bool
	launchBeforeACK atomic.Bool
	launchStarted   chan<- struct{}
	launchRelease   <-chan struct{}
}

type passwordButtonAuth struct {
	launches    atomic.Int32
	waitRelease <-chan struct{}
	waitDone    chan<- struct{}
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
	return nil
}

func (a *passwordButtonAuth) WaitPasswordLogin(context.Context, string) (string, string, string, error) {
	<-a.waitRelease
	if a.waitDone != nil {
		a.waitDone <- struct{}{}
	}
	return "", "", "", context.Canceled
}

func (*passwordButtonAuth) CompletePasswordMFA(context.Context, string, string) (string, error) {
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

func (*acknowledgementCheckingAuth) WaitPasswordLogin(context.Context, string) (string, string, string, error) {
	return "", "", "", nil
}

func (*acknowledgementCheckingAuth) CompletePasswordMFA(context.Context, string, string) (string, error) {
	return "", nil
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
}

func TestPasswordModalWaitsForOwnerCaptchaButtonBeforeLaunchingChrome(t *testing.T) {
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
	originalEdits := make(chan struct{}, 2)
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
	waitDone := make(chan struct{}, 1)
	auth := &passwordButtonAuth{waitRelease: waitRelease, waitDone: waitDone}
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

	captchaComponent := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-captcha-button",
		AppID: "application-1",
		Token: "token-captcha-button",
		Type:  discordgo.InteractionMessageComponent,
		Data:  discordgo.MessageComponentInteractionData{CustomID: customIDAuthCaptchaPref + "captcha-state"},
		User:  &discordgo.User{ID: "owner-1"},
	}}
	h.onComponent(session, captchaComponent)
	if got := auth.launches.Load(); got != 1 {
		t.Fatalf("owner button Chrome launches = %d, want 1", got)
	}
	select {
	case <-originalEdits:
	case <-time.After(time.Second):
		t.Fatal("captcha button response did not finish editing")
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
