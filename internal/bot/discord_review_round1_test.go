package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
)

type originalEditFailureTransport struct {
	status int
	err    error
}

func (t originalEditFailureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.URL.Path, "/callback") {
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     "204 No Content",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	}
	if t.err != nil {
		return nil, t.err
	}
	body := `{"message":"edit rejected","code":40060}`
	return &http.Response{
		StatusCode: t.status,
		Status:     fmt.Sprintf("%d %s", t.status, http.StatusText(t.status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}, nil
}

func newOriginalEditFailureSession(t *testing.T, failure originalEditFailureTransport) *discordgo.Session {
	t.Helper()
	originalCallbackEndpoint := discordgo.EndpointInteractionResponse
	originalWebhookEndpoint := discordgo.EndpointWebhookMessage
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return "https://discord.test/callback" }
	discordgo.EndpointWebhookMessage = func(_, _, _ string) string { return "https://discord.test/original" }
	t.Cleanup(func() {
		discordgo.EndpointInteractionResponse = originalCallbackEndpoint
		discordgo.EndpointWebhookMessage = originalWebhookEndpoint
	})
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	session.Client = &http.Client{Transport: failure}
	return session
}

func passwordModalInteractionForDeliveryTest(id string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: id, AppID: "application-1", Token: id + "-token", Type: discordgo.InteractionModalSubmit,
		Data: discordgo.ModalSubmitInteractionData{
			CustomID: customIDAuthPWModal,
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.TextInput{CustomID: "username", Value: "user"},
				discordgo.TextInput{CustomID: "password", Value: "pass"},
			}}},
		},
		User: &discordgo.User{ID: "owner-1"},
	}}
}

var editDeliveryFailures = []struct {
	name     string
	failure  originalEditFailureTransport
	definite bool
}{
	{name: "concrete Discord 400", failure: originalEditFailureTransport{status: http.StatusBadRequest}, definite: true},
	{name: "context canceled", failure: originalEditFailureTransport{err: context.Canceled}},
	{name: "EOF after possible commit", failure: originalEditFailureTransport{err: io.EOF}},
	{name: "connection reset after possible commit", failure: originalEditFailureTransport{err: syscall.ECONNRESET}},
}

// Mutation caught: treating every edit error as proven non-delivery destroys
// a password continuation after an ambiguous post-commit transport failure.
func TestPasswordModalRollsBackOnlyDefiniteEditRejection(t *testing.T) {
	for _, test := range editDeliveryFailures {
		t.Run(test.name, func(t *testing.T) {
			session := newOriginalEditFailureSession(t, test.failure)
			auth := &passwordButtonAuth{}
			h := &Handlers{Auth: auth}

			h.onModal(session, passwordModalInteractionForDeliveryTest("password-"+test.name))

			want := int32(0)
			if test.definite {
				want = 1
			}
			if got := auth.cancelCalls.Load(); got != want {
				t.Fatalf("password cancellation calls=%d, want %d", got, want)
			}
		})
	}
}

// Mutation caught: ambiguous launch-status delivery must retain the password
// state and enroll its watcher because Discord may have published the button.
func TestCaptchaLaunchRollsBackOnlyDefiniteEditRejection(t *testing.T) {
	for _, test := range editDeliveryFailures {
		t.Run(test.name, func(t *testing.T) {
			session := newOriginalEditFailureSession(t, test.failure)
			waitRelease := make(chan struct{})
			close(waitRelease)
			waitStarted := make(chan struct{}, 1)
			auth := &passwordButtonAuth{waitStarted: waitStarted, waitRelease: waitRelease}
			h := &Handlers{Auth: auth}
			t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
			interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
				ID: "captcha-" + test.name, AppID: "application-1", Token: "captcha-token",
				Type: discordgo.InteractionMessageComponent,
				Data: discordgo.MessageComponentInteractionData{CustomID: customIDAuthCaptchaPref + "captcha-state"},
				User: &discordgo.User{ID: "owner-1"},
			}}

			h.onComponent(session, interaction)

			wantCancel, wantWait := int32(0), int32(1)
			if test.definite {
				wantCancel, wantWait = 1, 0
			}
			if !test.definite {
				select {
				case <-waitStarted:
				case <-time.After(time.Second):
					t.Fatal("ambiguous launch edit did not enroll watcher")
				}
			}
			if got := auth.cancelCalls.Load(); got != wantCancel {
				t.Fatalf("password cancellation calls=%d, want %d", got, wantCancel)
			}
			if got := auth.waits.Load(); got != wantWait {
				t.Fatalf("password watcher calls=%d, want %d", got, wantWait)
			}
		})
	}
}

// Mutation caught: an ambiguous MFA-button edit can already be visible, so
// only a concrete Discord rejection is safe to owner-cancel its continuation.
func TestPasswordWatcherRollsBackMFAOnlyOnDefiniteEditRejection(t *testing.T) {
	for _, test := range editDeliveryFailures {
		t.Run(test.name, func(t *testing.T) {
			session := newOriginalEditFailureSession(t, test.failure)
			auth := &mfaDeliveryAuth{waitMFAState: "mfa-state", waitMFAHint: "a***@example.com"}
			h := &Handlers{Auth: auth}
			t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
			interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
				ID: "mfa-" + test.name, AppID: "application-1", Token: "mfa-token",
				User: &discordgo.User{ID: "owner-1"},
			}}

			h.watchPasswordCaptcha(context.Background(), session, interaction, "captcha-state", i18n.KO)

			want := int32(0)
			if test.definite {
				want = 1
			}
			if got := auth.cancelCalls.Load(); got != want {
				t.Fatalf("MFA cancellation calls=%d, want %d", got, want)
			}
		})
	}
}

type suppressedMFAWaitAuth struct {
	mfaDeliveryAuth
	mfaCreated chan struct{}
	release    chan struct{}
}

func (a *suppressedMFAWaitAuth) WaitPasswordLogin(context.Context, string) (string, string, string, error) {
	close(a.mfaCreated)
	<-a.release
	return "", "mfa-suppressed", "a***@example.com", nil
}

// Mutation caught: reporting a guarded, suppressed edit as successful retains
// an MFA continuation whose button was never sent to Discord.
func TestSuppressedPasswordWatcherEditRollsBackUndeliveredMFA(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	auth := &suppressedMFAWaitAuth{mfaCreated: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseWatcher := func() { releaseOnce.Do(func() { close(auth.release) }) }
	t.Cleanup(releaseWatcher)
	h := &Handlers{Auth: auth}
	h.mfaSubmissionGuard("mfa-suppressed")
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "interaction-suppressed-mfa", AppID: "application-1", Token: "token-suppressed-mfa",
		User: &discordgo.User{ID: "owner-1"},
	}}

	watcherDone := make(chan struct{})
	go func() {
		h.watchPasswordCaptcha(context.Background(), session, interaction, "captcha-state", i18n.KO)
		close(watcherDone)
	}()
	select {
	case <-auth.mfaCreated:
	case <-time.After(time.Second):
		t.Fatal("watcher did not create its MFA continuation")
	}

	winner := Response{Content: "expired", Components: []discordgo.MessageComponent{}}
	if err := h.editCaptchaInteractionContext(context.Background(), session, interaction, "captcha-state", winner, true); err != nil {
		t.Fatalf("apply winning terminal edit: %v", err)
	}
	select {
	case edit := <-capture.edits:
		if edit.Content != "expired" {
			t.Fatalf("winning terminal edit content=%q", edit.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("winning terminal edit was not delivered")
	}
	releaseWatcher()
	select {
	case <-watcherDone:
	case <-time.After(time.Second):
		t.Fatal("suppressed watcher did not finish")
	}

	if got := auth.cancelCalls.Load(); got != 1 {
		t.Fatalf("suppressed MFA cancellation calls=%d, want 1", got)
	}
	if got := h.mfaHintFor("mfa-suppressed"); got != "" {
		t.Fatalf("suppressed MFA retained cached hint %q", got)
	}
	h.mfaSubmitMu.Lock()
	_, guardExists := h.mfaSubmitGuards["mfa-suppressed"]
	h.mfaSubmitMu.Unlock()
	if guardExists {
		t.Fatal("suppressed MFA retained its submission guard")
	}
	if pending := len(capture.edits); pending != 0 {
		t.Fatalf("suppressed watcher emitted %d extra edit(s)", pending)
	}
}

type qrDeliveryAuth struct {
	passwordButtonAuth
	waits       atomic.Int32
	waitStarted chan struct{}
}

func (*qrDeliveryAuth) BeginQRAuth(context.Context, string) (string, string, error) {
	return "https://example.com/riot-mobile", "qr-state", nil
}

func (a *qrDeliveryAuth) WaitQRLogin(ctx context.Context, _ string) (string, error) {
	a.waits.Add(1)
	select {
	case a.waitStarted <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return "", ctx.Err()
}

// Mutation caught: a definite initial QR source-edit rejection must not leave
// an undeliverable watcher, while ambiguous transport failure retains it.
func TestQRWatcherStartsOnlyWhenInitialEditMayHaveDelivered(t *testing.T) {
	for _, test := range editDeliveryFailures {
		t.Run(test.name, func(t *testing.T) {
			session := newOriginalEditFailureSession(t, test.failure)
			auth := &qrDeliveryAuth{waitStarted: make(chan struct{}, 1)}
			h := &Handlers{Auth: auth}
			t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
			interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
				ID: "qr-" + test.name, AppID: "application-1", Token: "qr-token",
				Type: discordgo.InteractionMessageComponent,
				Data: discordgo.MessageComponentInteractionData{CustomID: customIDAuthQR},
				User: &discordgo.User{ID: "owner-1"},
			}}

			h.onComponent(session, interaction)

			if test.definite {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				if err := h.Shutdown(shutdownCtx); err != nil {
					cancel()
					t.Fatal(err)
				}
				cancel()
				if got := auth.waits.Load(); got != 0 {
					t.Fatalf("definite QR edit rejection started %d watcher(s)", got)
				}
				return
			}
			select {
			case <-auth.waitStarted:
			case <-time.After(time.Second):
				t.Fatal("ambiguous QR edit failure did not retain watcher")
			}
		})
	}
}

type naturalWatcherTimeoutAuth struct {
	passwordButtonAuth
}

func (*naturalWatcherTimeoutAuth) WaitQRLogin(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (*naturalWatcherTimeoutAuth) WaitPasswordLogin(ctx context.Context, _ string) (string, string, string, error) {
	<-ctx.Done()
	return "", "", "", ctx.Err()
}

// Mutation caught: reusing the naturally-expired Riot wait context for the QR
// terminal edit prevents the result from ever reaching Discord.
func TestQRWatcherNaturalTimeoutStillDeliversTerminalResult(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	h := &Handlers{Auth: &naturalWatcherTimeoutAuth{}}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "interaction-qr-natural-timeout", AppID: "application-1", Token: "token-qr-natural-timeout",
	}}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	h.watchQRLogin(waitCtx, session, interaction, "qr-state", i18n.KO)

	select {
	case edit := <-capture.edits:
		if edit.Content != i18n.T(i18n.KO, "auth.qr.timeout") {
			t.Fatalf("QR timeout edit content=%q", edit.Content)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("natural QR timeout did not retain a terminal-delivery window")
	}
}

// Mutation caught: reusing the naturally-expired CAPTCHA wait context for the
// password terminal edit leaves the CAPTCHA controls stale.
func TestPasswordWatcherNaturalTimeoutStillDeliversTerminalResult(t *testing.T) {
	session, capture := newMFAInteractionCapture(t, nil)
	h := &Handlers{Auth: &naturalWatcherTimeoutAuth{}}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "interaction-password-natural-timeout", AppID: "application-1", Token: "token-password-natural-timeout",
		User: &discordgo.User{ID: "owner-1"},
	}}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	h.watchPasswordCaptcha(waitCtx, session, interaction, "captcha-state", i18n.KO)

	select {
	case edit := <-capture.edits:
		if edit.Content != i18n.T(i18n.KO, "auth.captcha.timeout") {
			t.Fatalf("password timeout edit content=%q", edit.Content)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("natural password timeout did not retain a terminal-delivery window")
	}
}

func drainFailedHandlerShutdown(t *testing.T, h *Handlers, workerDone <-chan struct{}, firstErr error) {
	t.Helper()
	if firstErr == nil {
		return
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.Shutdown(drainCtx); err != nil {
		t.Fatalf("drain handler shutdown after first error: %v", err)
	}
	select {
	case <-workerDone:
	case <-drainCtx.Done():
		t.Fatal("Discord delivery worker did not drain during test cleanup")
	}
}

// Mutation caught: sending an interaction through the source Session's shared
// discordgo bucket makes lifecycle cancellation wait for its context-blind sleep.
func TestHandlerShutdownDrainsInteractionDeliveryWithBlockedDiscordBucket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset-After", "0.25")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	originalEndpoint := discordgo.EndpointInteractionResponse
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	defer func() { discordgo.EndpointInteractionResponse = originalEndpoint }()
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "interaction-bucket-wait", AppID: "application-1", Token: "token-bucket-wait",
	}}
	if err := deferInteraction(context.Background(), session, interaction, false); err != nil {
		t.Fatalf("prime Discord bucket: %v", err)
	}

	h := &Handlers{}
	workerCtx, workerDone, ok := h.beginLifecycleWorker(time.Second)
	if !ok {
		t.Fatal("could not enroll delivery worker")
	}
	entered := make(chan struct{})
	deliveryDone := make(chan struct{})
	go func() {
		close(entered)
		_ = deferInteraction(workerCtx, session, interaction, false)
		workerDone()
		close(deliveryDone)
	}()
	<-entered

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	shutdownErr := h.Shutdown(shutdownCtx)
	cancel()
	drainFailedHandlerShutdown(t, h, deliveryDone, shutdownErr)
	if shutdownErr != nil {
		t.Fatalf("Shutdown waited on discordgo's context-blind bucket: %v", shutdownErr)
	}
}

// Mutation caught: allowing discordgo's default 429 retry makes handler
// shutdown wait through Retry-After even after the lifecycle context is canceled.
func TestHandlerShutdownCancelsAndDrainsDiscordRateLimitRetry(t *testing.T) {
	requestSeen := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case requestSeen <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"retry_after":0.25,"global":false}`)
	}))
	defer srv.Close()
	originalEndpoint := discordgo.EndpointInteractionResponse
	discordgo.EndpointInteractionResponse = func(_, _ string) string { return srv.URL + "/callback" }
	defer func() { discordgo.EndpointInteractionResponse = originalEndpoint }()
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	rateLimitEntered := make(chan struct{}, 1)
	session.AddHandler(func(_ *discordgo.Session, _ *discordgo.RateLimit) {
		select {
		case rateLimitEntered <- struct{}{}:
		default:
		}
	})
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "interaction-rate-limit", AppID: "application-1", Token: "token-rate-limit",
	}}
	h := &Handlers{}
	workerCtx, workerDone, ok := h.beginLifecycleWorker(time.Second)
	if !ok {
		t.Fatal("could not enroll delivery worker")
	}
	deliveryCtx, cancelDelivery := context.WithTimeout(workerCtx, 100*time.Millisecond)
	defer cancelDelivery()
	deliveryDone := make(chan struct{})
	go func() {
		_ = deferInteraction(deliveryCtx, session, interaction, false)
		workerDone()
		close(deliveryDone)
	}()
	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("Discord 429 response was not reached")
	}
	// With discordgo's default retry, this event is emitted immediately before
	// its context-blind sleep. The bounded delivery path instead outlives no
	// more than deliveryCtx, so waiting for either branch remains deterministic.
	select {
	case <-rateLimitEntered:
		cancelDelivery()
	case <-deliveryCtx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	shutdownErr := h.Shutdown(shutdownCtx)
	cancel()
	drainFailedHandlerShutdown(t, h, deliveryDone, shutdownErr)
	if shutdownErr != nil {
		t.Fatalf("Shutdown waited through discordgo's context-blind 429 retry: %v", shutdownErr)
	}
}
