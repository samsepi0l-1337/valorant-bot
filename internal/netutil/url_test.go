package netutil_test

import (
	"testing"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
)

func TestIsPrivateOrLocalAuthBaseURL(t *testing.T) {
	cases := []struct {
		in     string
		want   bool
	}{
		{"http://192.168.0.37:8787", true},
		{"http://10.0.0.5:8787", true},
		{"http://127.0.0.1:8787", true},
		{"http://localhost:8787", true},
		{"http://raspberrypi.local:8787", true},
		{"https://bot.example.com", false},
		{"https://random-name.trycloudflare.com", false},
		{"", true},
	}
	for _, tc := range cases {
		if got := netutil.IsPrivateOrLocalAuthBaseURL(tc.in); got != tc.want {
			t.Fatalf("%q: got %v want %v", tc.in, got, tc.want)
		}
	}
}
