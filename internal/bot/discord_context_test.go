package bot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
)

type blockingDiscordRequest struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingDiscordSession(t *testing.T, blockedPath string) (*discordgo.Session, *blockingDiscordRequest) {
	t.Helper()
	blocked := &blockingDiscordRequest{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == blockedPath {
			select {
			case blocked.started <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
				return
			case <-blocked.release:
			}
		}
		if r.URL.Path == "/original" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	originalCallbackEndpoint := discordgo.EndpointInteractionResponse
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return srv.URL + "/original" }
	release := func() { blocked.once.Do(func() { close(blocked.release) }) }
	t.Cleanup(func() {
		release()
		discordgo.EndpointInteractionResponse = originalCallbackEndpoint
		discordgo.EndpointWebhookMessage = originalWebhookEndpoint
		srv.Close()
	})

	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	return session, blocked
}

func requireBlockedDiscordRequest(t *testing.T, blocked *blockingDiscordRequest) {
	t.Helper()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		blocked.once.Do(func() { close(blocked.release) })
		t.Fatal("Discord REST request did not reach the blocking HTTP endpoint")
	}
}

// Mutation caught: omitting discordgo.WithContext from the initial interaction
// acknowledgement keeps OnInteraction enrolled after its lifecycle is canceled.
func TestHandlersShutdownCancelsBlockedInteractionAcknowledgement(t *testing.T) {
	session, blocked := newBlockingDiscordSession(t, "/callback")
	h := &Handlers{}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:    "interaction-blocked-ack",
		AppID: "application-1",
		Token: "token-blocked-ack",
		Type:  discordgo.InteractionApplicationCommand,
		Data:  discordgo.ApplicationCommandInteractionData{Name: "auth"},
		User:  &discordgo.User{ID: "owner-1"},
	}}
	callbackDone := make(chan struct{})
	go func() {
		h.OnInteraction(session, interaction)
		close(callbackDone)
	}()
	requireBlockedDiscordRequest(t, blocked)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	shutdownErr := h.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		blocked.once.Do(func() { close(blocked.release) })
		<-callbackDone
		t.Fatalf("Shutdown error=%v, want blocked ACK cancellation and worker drain", shutdownErr)
	}
	select {
	case <-callbackDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("interaction callback remained blocked after Shutdown")
	}
}

// Mutation caught: dropping the QR watcher's context from its terminal source
// edit prevents lifecycle shutdown from aborting the real webhook request.
func TestHandlersShutdownCancelsBlockedQRWatcherTerminalEdit(t *testing.T) {
	session, blocked := newBlockingDiscordSession(t, "/original")
	h := &Handlers{Auth: &passwordButtonAuth{}}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "interaction-blocked-qr-edit", AppID: "application-1", Token: "token-blocked-qr-edit",
	}}
	h.startQRLoginWatcher(session, interaction, "qr-state", i18n.KO)
	requireBlockedDiscordRequest(t, blocked)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	shutdownErr := h.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		blocked.once.Do(func() { close(blocked.release) })
		_ = h.Shutdown(context.Background())
		t.Fatalf("Shutdown error=%v, want blocked QR terminal edit cancellation", shutdownErr)
	}
}

// Mutation caught: dropping the password watcher's context from its terminal
// source edit leaves the only CAPTCHA/MFA result request alive during shutdown.
func TestHandlersShutdownCancelsBlockedPasswordWatcherTerminalEdit(t *testing.T) {
	session, blocked := newBlockingDiscordSession(t, "/original")
	waitRelease := make(chan struct{})
	close(waitRelease)
	h := &Handlers{Auth: &passwordButtonAuth{waitRelease: waitRelease}}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "interaction-blocked-password-edit", AppID: "application-1", Token: "token-blocked-password-edit",
		User: &discordgo.User{ID: "owner-1"},
	}}
	h.startPasswordCaptchaWatcher(session, interaction, "captcha-state", i18n.KO)
	requireBlockedDiscordRequest(t, blocked)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	shutdownErr := h.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		blocked.once.Do(func() { close(blocked.release) })
		_ = h.Shutdown(context.Background())
		t.Fatalf("Shutdown error=%v, want blocked password terminal edit cancellation", shutdownErr)
	}
}
