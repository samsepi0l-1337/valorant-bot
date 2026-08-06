package valorantbot

import "testing"

func TestIsLoopbackAuthBase(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:8787", true},
		{"http://localhost:8787", true},
		{"http://[::1]:8787", true},
		{"http://192.168.0.10:8787", false},
		{"http://raspberrypi.local:8787", false},
		{"https://bot.example.com", false},
		{"", false},
		{"not a url", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAuthBase(tc.url); got != tc.want {
			t.Errorf("isLoopbackAuthBase(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
