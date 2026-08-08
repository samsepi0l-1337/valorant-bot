package bot

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
)

type dispatchCountingTransport struct {
	calls atomic.Int32
}

// Mutation caught: skipping a watcher after a definite initial QR edit failure
// without owner-canceling the created flow leaks its persisted pending state.
func TestInitialQRDefiniteNonDeliveryCancelsOnlyOwnedFlow(t *testing.T) {
	for _, test := range editDeliveryFailures {
		t.Run(test.name, func(t *testing.T) {
			session := newOriginalEditFailureSession(t, test.failure)
			auth := &qrDeliveryAuth{waitStarted: make(chan struct{}, 1)}
			h := &Handlers{Auth: auth}
			t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
			interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
				ID: "qr-cleanup-" + test.name, AppID: "application-1", Token: "qr-cleanup-token",
				Type: discordgo.InteractionMessageComponent,
				Data: discordgo.MessageComponentInteractionData{CustomID: customIDAuthQR},
				User: &discordgo.User{ID: "owner-1"},
			}}

			h.onComponent(session, interaction)

			wantCancel := int32(0)
			if test.definite {
				wantCancel = 1
			} else {
				select {
				case <-auth.waitStarted:
				case <-time.After(time.Second):
					t.Fatal("ambiguous QR delivery did not retain its watcher")
				}
			}
			if got := auth.cancelCalls.Load(); got != wantCancel {
				t.Fatalf("QR cancellation calls=%d, want %d", got, wantCancel)
			}
			if test.definite {
				auth.cancelMu.Lock()
				state, user := auth.cancelState, auth.cancelUser
				auth.cancelMu.Unlock()
				if state != "qr-state" || user != "owner-1" {
					t.Fatalf("QR cancellation=(%q,%q), want owner-bound flow", state, user)
				}
			}
		})
	}
}

func (t *dispatchCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("unexpected request dispatch")
}

func newDispatchCountingSession(t *testing.T) (*discordgo.Session, *dispatchCountingTransport) {
	t.Helper()
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	transport := &dispatchCountingTransport{}
	session.Client = &http.Client{Transport: transport}
	return session, transport
}

// Mutation caught: classifying every context cancellation as ambiguous retains
// state even when cancellation prevented the request from entering RoundTrip.
func TestInteractionEditPreCanceledContextIsDefiniteNonDelivery(t *testing.T) {
	session, transport := newDispatchCountingSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	outcome, err := editInteractionOutcome(ctx, session, passwordModalInteractionForDeliveryTest("pre-canceled"), Response{Content: "terminal"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("edit error=%v, want context.Canceled", err)
	}
	if outcome != deliveryRejected {
		t.Fatalf("delivery outcome=%v, want definite rejection", outcome)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("RoundTrip calls=%d, want 0", got)
	}
}

// Mutation caught: returning an ambiguous outcome when a canceled waiter never
// acquired the terminal edit guard preserves a continuation that had no edit.
func TestCanceledTerminalGuardWaitIsDefiniteNonDelivery(t *testing.T) {
	h := &Handlers{}
	guard := h.captchaEditGuard("guarded-state")
	acquired, err := guard.begin(context.Background())
	if err != nil || !acquired {
		t.Fatalf("hold edit guard: acquired=%v err=%v", acquired, err)
	}
	defer guard.finish(false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := h.editCaptchaInteractionOutcome(ctx, nil, nil, "guarded-state", Response{Content: "terminal"}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("guard wait error=%v, want context.Canceled", err)
	}
	if outcome != deliveryRejected {
		t.Fatalf("delivery outcome=%v, want definite rejection", outcome)
	}
}

// Mutation caught: returning from a watcher when terminal-delivery enrollment
// is already closed leaks the newly-created MFA state whose button was never sent.
func TestPasswordWatcherEnrollmentFailureRollsBackUndeliveredMFA(t *testing.T) {
	auth := &mfaDeliveryAuth{waitMFAState: "mfa-before-worker", waitMFAHint: "a***@example.com"}
	h := &Handlers{Auth: auth}
	h.lifecycleMu.Lock()
	h.lifecycleClosed = true
	h.lifecycleMu.Unlock()
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "worker-enrollment-failed", AppID: "application-1", Token: "worker-token",
		User: &discordgo.User{ID: "owner-1"},
	}}

	h.watchPasswordCaptcha(context.Background(), nil, interaction, "captcha-state", i18n.KO)

	if got := auth.cancelCalls.Load(); got != 1 {
		t.Fatalf("MFA cancellation calls=%d, want 1", got)
	}
	if got := h.mfaHintFor("mfa-before-worker"); got != "" {
		t.Fatalf("failed delivery enrollment retained MFA hint %q", got)
	}
}
