package bot

import (
	"context"
	"errors"
	"net/http"

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

// interactionDeliveryResult only declares non-delivery when Discord returned
// a concrete client rejection. Transport errors and cancellation can happen
// after Discord committed the edit, so rolling back state for them can strand
// a control that is already visible to the user.
func interactionDeliveryResult(err error) interactionDeliveryOutcome {
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

// deliverInteraction isolates interaction callbacks from the gateway session's
// shared rate-limit buckets. discordgo acquires those buckets before applying
// WithContext and performs its built-in 429 retry with time.Sleep, so neither
// wait is cancelable on the long-lived session. A fresh session supplies a
// fresh limiter without copying discordgo's mutex-bearing Session value, while
// the request context still cancels the underlying HTTP exchange.
func deliverInteraction(ctx context.Context, source *discordgo.Session, request interactionDiscordRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if source == nil {
		return errors.New("discord interaction delivery: nil session")
	}

	delivery, err := discordgo.New(source.Token)
	if err != nil {
		return err
	}
	// Client and UserAgent are immutable configuration for these requests. In
	// particular, never copy the Session itself or its shared Ratelimiter.
	if source.Client != nil {
		delivery.Client = source.Client
	}
	if source.UserAgent != "" {
		delivery.UserAgent = source.UserAgent
	}
	delivery.ShouldRetryOnRateLimit = false
	delivery.MaxRestRetries = 0

	return request(delivery,
		discordgo.WithContext(ctx),
		discordgo.WithRetryOnRatelimit(false),
		discordgo.WithRestRetries(0),
	)
}

func interactionRespond(ctx context.Context, s *discordgo.Session, interaction *discordgo.Interaction, response *discordgo.InteractionResponse) error {
	return deliverInteraction(ctx, s, func(delivery *discordgo.Session, options ...discordgo.RequestOption) error {
		return delivery.InteractionRespond(interaction, response, options...)
	})
}

func interactionEdit(ctx context.Context, s *discordgo.Session, interaction *discordgo.Interaction, edit *discordgo.WebhookEdit) error {
	return deliverInteraction(ctx, s, func(delivery *discordgo.Session, options ...discordgo.RequestOption) error {
		_, err := delivery.InteractionResponseEdit(interaction, edit, options...)
		return err
	})
}
