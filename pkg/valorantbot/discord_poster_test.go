package valorantbot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type blockingDiscordTransport struct {
	blockRequest int32
	requests     atomic.Int32
	started      chan struct{}
	release      chan struct{}
	startedOnce  sync.Once
	releaseOnce  sync.Once
}

func (t *blockingDiscordTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.requests.Add(1) < t.blockRequest {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"dm-channel"}`)),
			Request:    req,
		}, nil
	}

	t.startedOnce.Do(func() { close(t.started) })
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-t.release:
		return nil, errors.New("test transport released")
	}
}

func (t *blockingDiscordTransport) unblock() {
	t.releaseOnce.Do(func() { close(t.release) })
}

func TestDiscordPosterCancelsRESTRequestsWithCallerContext(t *testing.T) {
	tests := []struct {
		name         string
		blockRequest int32
		invoke       func(context.Context, *discordPoster) error
	}{
		{
			name:         "channel message",
			blockRequest: 1,
			invoke: func(ctx context.Context, poster *discordPoster) error {
				return poster.PostChannel(ctx, "channel", "content", nil)
			},
		},
		{
			name:         "DM channel creation",
			blockRequest: 1,
			invoke: func(ctx context.Context, poster *discordPoster) error {
				return poster.SendDM(ctx, "user", "content", nil)
			},
		},
		{
			name:         "DM channel message",
			blockRequest: 2,
			invoke: func(ctx context.Context, poster *discordPoster) error {
				return poster.SendDM(ctx, "user", "content", nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &blockingDiscordTransport{
				blockRequest: tt.blockRequest,
				started:      make(chan struct{}),
				release:      make(chan struct{}),
			}
			t.Cleanup(transport.unblock)
			session, err := discordgo.New("Bot test-token")
			if err != nil {
				t.Fatal(err)
			}
			session.Client = &http.Client{Transport: transport}
			poster := &discordPoster{session: session}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tt.invoke(ctx, poster) }()
			select {
			case <-transport.started:
			case <-time.After(2 * time.Second):
				transport.unblock()
				t.Fatal("Discord REST request did not start")
			}

			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("poster error = %v, want context canceled", err)
				}
			case <-time.After(100 * time.Millisecond):
				transport.unblock()
				<-done
				t.Fatal("poster did not cancel its in-flight Discord REST request")
			}
		})
	}
}
