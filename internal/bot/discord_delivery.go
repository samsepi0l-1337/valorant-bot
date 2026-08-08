package bot

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/bwmarrin/discordgo"
)

type interactionDiscordRequest func(*discordgo.Session, ...discordgo.RequestOption) error

type interactionDeliveryOutcome uint8

const (
	deliveryApplied interactionDeliveryOutcome = iota
	deliverySuppressed
	deliveryRejected
	deliveryAmbiguous
)

// interactionDeliveryResult records whether a request left the process. A
// pre-dispatch failure is proven non-delivery; a transport error after
// RoundTrip begins remains ambiguous because Discord may have committed it.
type interactionDeliveryResult struct {
	outcome           interactionDeliveryOutcome
	err               error
	requestDispatched bool
}

func classifyDispatchedInteractionError(err error) interactionDeliveryOutcome {
	if err == nil {
		return deliveryApplied
	}
	var rateLimitErr *discordgo.RateLimitError
	if errors.As(err, &rateLimitErr) {
		return deliveryRejected
	}
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr != nil && restErr.Response != nil {
		if status := restErr.Response.StatusCode; status >= http.StatusBadRequest && status < http.StatusInternalServerError {
			return deliveryRejected
		}
	}
	return deliveryAmbiguous
}

type dispatchTrackingRoundTripper struct {
	base       http.RoundTripper
	dispatched *atomic.Bool
}

func (t dispatchTrackingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	t.dispatched.Store(true)
	return t.base.RoundTrip(request)
}

// deliverInteraction isolates interaction callbacks from the gateway session's
// shared rate-limit buckets. discordgo acquires those buckets before applying
// WithContext and performs its built-in 429 retry with time.Sleep, so neither
// wait is cancelable on the long-lived session. A fresh session supplies a
// fresh limiter without copying discordgo's mutex-bearing Session value, while
// the request context still cancels the underlying HTTP exchange.
func deliverInteraction(ctx context.Context, source *discordgo.Session, request interactionDiscordRequest) interactionDeliveryResult {
	if err := ctx.Err(); err != nil {
		return interactionDeliveryResult{outcome: deliveryRejected, err: err}
	}
	if source == nil {
		return interactionDeliveryResult{outcome: deliveryRejected, err: errors.New("discord interaction delivery: nil session")}
	}

	delivery, err := discordgo.New(source.Token)
	if err != nil {
		return interactionDeliveryResult{outcome: deliveryRejected, err: err}
	}
	// Client and UserAgent are immutable configuration for these requests. In
	// particular, never copy the Session itself or its shared Ratelimiter.
	client := delivery.Client
	if source.Client != nil {
		client = source.Client
	}
	baseTransport := client.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	var dispatched atomic.Bool
	delivery.Client = &http.Client{
		Transport:     dispatchTrackingRoundTripper{base: baseTransport, dispatched: &dispatched},
		CheckRedirect: client.CheckRedirect,
		Jar:           client.Jar,
		Timeout:       client.Timeout,
	}
	if source.UserAgent != "" {
		delivery.UserAgent = source.UserAgent
	}
	delivery.ShouldRetryOnRateLimit = false
	delivery.MaxRestRetries = 0

	err = request(delivery,
		discordgo.WithContext(ctx),
		discordgo.WithRetryOnRatelimit(false),
		discordgo.WithRestRetries(0),
	)
	requestDispatched := dispatched.Load()
	outcome := deliveryRejected
	if err == nil {
		outcome = deliveryApplied
	} else if requestDispatched {
		outcome = classifyDispatchedInteractionError(err)
	}
	return interactionDeliveryResult{outcome: outcome, err: err, requestDispatched: requestDispatched}
}

func interactionRespond(ctx context.Context, s *discordgo.Session, interaction *discordgo.Interaction, response *discordgo.InteractionResponse) error {
	result := deliverInteraction(ctx, s, func(delivery *discordgo.Session, options ...discordgo.RequestOption) error {
		return delivery.InteractionRespond(interaction, response, options...)
	})
	return result.err
}

func interactionEdit(ctx context.Context, s *discordgo.Session, interaction *discordgo.Interaction, edit *discordgo.WebhookEdit) error {
	return interactionEditResult(ctx, s, interaction, edit).err
}

func interactionEditResult(ctx context.Context, s *discordgo.Session, interaction *discordgo.Interaction, edit *discordgo.WebhookEdit) interactionDeliveryResult {
	return deliverInteraction(ctx, s, func(delivery *discordgo.Session, options ...discordgo.RequestOption) error {
		_, err := delivery.InteractionResponseEdit(interaction, edit, options...)
		return err
	})
}
