package bot

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestDiscordRESTErrorLogOmitsSecretsAndBodies(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		want      string
		forbidden []string
	}{
		{
			name: "transport URL",
			err: &url.Error{
				Op:  http.MethodPost,
				URL: "https://discord.com/api/v10/interactions/123/SENTINEL_INTERACTION_TOKEN/callback",
				Err: errors.New("connection refused"),
			},
			want:      "discord REST error type=*url.Error",
			forbidden: []string{"https://discord.com", "SENTINEL_INTERACTION_TOKEN"},
		},
		{
			name: "rate limit endpoint",
			err: &discordgo.RateLimitError{RateLimit: &discordgo.RateLimit{
				TooManyRequests: &discordgo.TooManyRequests{
					Message:    "SENTINEL_RATE_LIMIT_BODY",
					RetryAfter: 2 * time.Second,
				},
				URL: "https://discord.com/api/v10/webhooks/application/SENTINEL_WEBHOOK_TOKEN/messages/@original",
			}},
			want: "discord REST error type=*discordgo.RateLimitError status=429 retry_after=2s",
			forbidden: []string{
				"https://discord.com",
				"SENTINEL_WEBHOOK_TOKEN",
				"SENTINEL_RATE_LIMIT_BODY",
			},
		},
		{
			name: "REST response body",
			err: &discordgo.RESTError{
				Request: &http.Request{URL: &url.URL{
					Scheme: "https",
					Host:   "discord.com",
					Path:   "/api/v10/webhooks/application/SENTINEL_REST_TOKEN/messages/@original",
				}},
				Response:     &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"},
				ResponseBody: []byte(`{"code":50035,"message":"SENTINEL_RESPONSE_BODY"}`),
				Message:      &discordgo.APIErrorMessage{Code: 50035, Message: "SENTINEL_API_MESSAGE"},
			},
			want: "discord REST error type=*discordgo.RESTError status=400 code=50035",
			forbidden: []string{
				"discord.com",
				"SENTINEL_REST_TOKEN",
				"SENTINEL_RESPONSE_BODY",
				"SENTINEL_API_MESSAGE",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := discordRESTErrorLog(tt.err)
			if got != tt.want {
				t.Fatalf("discordRESTErrorLog() = %q, want %q", got, tt.want)
			}
			for _, secret := range tt.forbidden {
				if strings.Contains(got, secret) {
					t.Errorf("discordRESTErrorLog() leaked %q in %q", secret, got)
				}
			}
		})
	}
}
