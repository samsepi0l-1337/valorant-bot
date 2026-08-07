package valorantbot

import (
	"testing"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
)

func TestIsPrivateOrLocalAuthBaseURL_ForPublicPages(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:8787", true},
		{"http://localhost:8787", true},
		{"http://[::1]:8787", true},
		{"http://192.168.0.10:8787", true},
		{"http://raspberrypi.local:8787", true},
		{"https://bot.example.com", false},
		{"https://abc.trycloudflare.com", false},
		{"", true},
	}
	for _, tc := range cases {
		if got := netutil.IsPrivateOrLocalAuthBaseURL(tc.url); got != tc.want {
			t.Errorf("IsPrivateOrLocalAuthBaseURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
