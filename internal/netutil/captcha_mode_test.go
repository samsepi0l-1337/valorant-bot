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
		{name: "remote root path", rawMode: "remote", authBaseURL: "https://relay.example.com/", want: netutil.CaptchaBrowserRemote},
		{name: "remote DNS port", rawMode: "remote", authBaseURL: "https://relay.example.com:8443", want: netutil.CaptchaBrowserRemote},
		{name: "remote IPv4 port", rawMode: "remote", authBaseURL: "https://192.0.2.10:443", want: netutil.CaptchaBrowserRemote},
		{name: "remote bracketed IPv6 port", rawMode: "remote", authBaseURL: "https://[2001:db8::1]:443", want: netutil.CaptchaBrowserRemote},
		{name: "disabled", rawMode: "disabled", authBaseURL: "http://bot.example.com", want: netutil.CaptchaBrowserDisabled},
		{name: "mixed case", rawMode: " ReMoTe ", authBaseURL: "https://relay.example.com", want: netutil.CaptchaBrowserRemote},
		{name: "unsupported", rawMode: "browser", authBaseURL: "https://relay.example.com", wantErr: true},
		{name: "remote HTTP origin", rawMode: "remote", authBaseURL: "http://relay.example.com", wantErr: true},
		{name: "remote HTTPS path", rawMode: "remote", authBaseURL: "https://relay.example.com/relay", wantErr: true},
		{name: "remote HTTPS query", rawMode: "remote", authBaseURL: "https://relay.example.com?token=abc", wantErr: true},
		{name: "remote HTTPS fragment", rawMode: "remote", authBaseURL: "https://relay.example.com#fragment", wantErr: true},
		{name: "remote HTTPS user info", rawMode: "remote", authBaseURL: "https://user:pass@relay.example.com", wantErr: true},
		{name: "remote surrounding leading whitespace", rawMode: "remote", authBaseURL: " https://relay.example.com", wantErr: true},
		{name: "remote surrounding trailing whitespace", rawMode: "remote", authBaseURL: "https://relay.example.com ", wantErr: true},
		{name: "remote empty query delimiter", rawMode: "remote", authBaseURL: "https://relay.example.com?", wantErr: true},
		{name: "remote empty fragment delimiter", rawMode: "remote", authBaseURL: "https://relay.example.com#", wantErr: true},
		{name: "remote escaped non root path", rawMode: "remote", authBaseURL: "https://relay.example.com/%2e", wantErr: true},
		{name: "remote dangling port", rawMode: "remote", authBaseURL: "https://relay.example.com:", wantErr: true},
		{name: "remote non numeric port", rawMode: "remote", authBaseURL: "https://relay.example.com:abc", wantErr: true},
		{name: "remote zero port", rawMode: "remote", authBaseURL: "https://relay.example.com:0", wantErr: true},
		{name: "remote negative port", rawMode: "remote", authBaseURL: "https://relay.example.com:-1", wantErr: true},
		{name: "remote overflowing port", rawMode: "remote", authBaseURL: "https://relay.example.com:65536", wantErr: true},
		{name: "remote malformed bracket host", rawMode: "remote", authBaseURL: "https://[::1", wantErr: true},
		{name: "remote malformed IPv6", rawMode: "remote", authBaseURL: "https://[2001:db8:::1]", wantErr: true},
		{name: "remote zero-padded IPv4", rawMode: "remote", authBaseURL: "https://192.168.001.1", wantErr: true},
		{name: "remote abbreviated IPv4", rawMode: "remote", authBaseURL: "https://127.1", wantErr: true},
		{name: "remote hexadecimal IPv4", rawMode: "remote", authBaseURL: "https://0x7f.0x0.0x0.0x1", wantErr: true},
		{name: "remote IPv6 zone", rawMode: "remote", authBaseURL: "https://[fe80::1%25eth0]", wantErr: true},
		{name: "remote dotted IPv4-mapped IPv6", rawMode: "remote", authBaseURL: "https://[::ffff:192.0.2.1]", wantErr: true},
		{name: "remote hexadecimal IPv4-mapped IPv6 port", rawMode: "remote", authBaseURL: "https://[::ffff:c000:201]:8443", wantErr: true},
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

func TestCanonicalRemoteCaptchaOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "DNS and scheme case with default port", raw: "HTTPS://Relay.Example.COM:443/", want: "https://relay.example.com"},
		{name: "DNS zero-padded default port", raw: "https://relay.example.com:0443", want: "https://relay.example.com"},
		{name: "DNS nondefault port", raw: "https://Relay.Example.COM:8443", want: "https://relay.example.com:8443"},
		{name: "DNS zero-padded nondefault port", raw: "https://Relay.Example.COM:08443", want: "https://relay.example.com:8443"},
		{name: "IPv4 default port", raw: "https://192.0.2.10:443", want: "https://192.0.2.10"},
		{name: "IPv4 nondefault port", raw: "https://192.0.2.10:8443", want: "https://192.0.2.10:8443"},
		{name: "IPv6 spelling and default port", raw: "https://[2001:0DB8:0:0:0:0:0:1]:443", want: "https://[2001:db8::1]"},
		{name: "IPv6 nondefault port", raw: "https://[2001:0DB8::1]:8443", want: "https://[2001:db8::1]:8443"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := netutil.CanonicalRemoteCaptchaOrigin(test.raw)
			if err != nil {
				t.Fatalf("CanonicalRemoteCaptchaOrigin(%q): %v", test.raw, err)
			}
			if got != test.want {
				t.Fatalf("CanonicalRemoteCaptchaOrigin(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}
