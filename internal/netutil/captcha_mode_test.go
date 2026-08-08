package netutil_test

import (
	"testing"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
)

func TestNormalizeCaptchaBrowserMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		rawMode     string
		authBaseURL string
		want        netutil.CaptchaBrowserMode
		wantErr     bool
	}{
		{name: "empty defaults local", authBaseURL: "http://bot.example.com", want: netutil.CaptchaBrowserLocal},
		{name: "local", rawMode: "local", authBaseURL: "http://bot.example.com", want: netutil.CaptchaBrowserLocal},
		{name: "remote", rawMode: "remote", authBaseURL: "https://relay.example.com", want: netutil.CaptchaBrowserRemote},
		{name: "disabled", rawMode: "disabled", authBaseURL: "http://bot.example.com", want: netutil.CaptchaBrowserDisabled},
		{name: "mixed case", rawMode: " ReMoTe ", authBaseURL: "https://relay.example.com", want: netutil.CaptchaBrowserRemote},
		{name: "unsupported", rawMode: "browser", authBaseURL: "https://relay.example.com", wantErr: true},
		{name: "remote HTTP origin", rawMode: "remote", authBaseURL: "http://relay.example.com", wantErr: true},
		{name: "remote HTTPS path", rawMode: "remote", authBaseURL: "https://relay.example.com/relay", wantErr: true},
		{name: "remote HTTPS query", rawMode: "remote", authBaseURL: "https://relay.example.com?token=abc", wantErr: true},
		{name: "remote HTTPS fragment", rawMode: "remote", authBaseURL: "https://relay.example.com#fragment", wantErr: true},
		{name: "remote HTTPS user info", rawMode: "remote", authBaseURL: "https://user:pass@relay.example.com", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := netutil.NormalizeCaptchaBrowserMode(tt.rawMode, tt.authBaseURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeCaptchaBrowserMode(%q, %q) error = nil", tt.rawMode, tt.authBaseURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeCaptchaBrowserMode(%q, %q): %v", tt.rawMode, tt.authBaseURL, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeCaptchaBrowserMode(%q, %q) = %q, want %q", tt.rawMode, tt.authBaseURL, got, tt.want)
			}
		})
	}
}
