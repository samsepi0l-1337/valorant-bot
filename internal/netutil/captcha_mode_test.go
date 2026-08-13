package netutil_test

import (
	"strings"
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
		{name: "remote HTTP public IPv4", rawMode: "remote", authBaseURL: "http://192.0.2.10:8787", wantErr: true},
		{name: "remote HTTP public DNS", rawMode: "remote", authBaseURL: "http://bot.example.com", wantErr: true},
		{name: "remote HTTP path", rawMode: "remote", authBaseURL: "http://192.168.0.10:8787/relay", wantErr: true},
		{name: "remote HTTP query", rawMode: "remote", authBaseURL: "http://192.168.0.10:8787?token=abc", wantErr: true},
		{name: "remote HTTP user info", rawMode: "remote", authBaseURL: "http://user:pass@192.168.0.10:8787", wantErr: true},
		{name: "remote HTTP LAN IPv4", rawMode: "remote", authBaseURL: "http://192.168.0.10:8787", want: netutil.CaptchaBrowserRemote},
		{name: "remote HTTP loopback default port", rawMode: "remote", authBaseURL: "http://127.0.0.1:80", want: netutil.CaptchaBrowserRemote},
		{name: "remote HTTP localhost", rawMode: "remote", authBaseURL: "http://localhost:8787", want: netutil.CaptchaBrowserRemote},
		{name: "remote HTTP mDNS", rawMode: "remote", authBaseURL: "http://raspberrypi.local:8787", want: netutil.CaptchaBrowserRemote},
		{name: "remote HTTP internal DNS", rawMode: "remote", authBaseURL: "http://bot.internal:8787", want: netutil.CaptchaBrowserRemote},
		{name: "remote HTTP link-local IPv4", rawMode: "remote", authBaseURL: "http://169.254.1.1:8787", want: netutil.CaptchaBrowserRemote},
		{name: "remote HTTP unique local IPv6", rawMode: "remote", authBaseURL: "http://[fd12:3456:789a::1]:8787", want: netutil.CaptchaBrowserRemote},
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
		{name: "HTTP LAN IPv4 nondefault port", raw: "http://192.168.0.10:8787", want: "http://192.168.0.10:8787"},
		{name: "HTTP LAN IPv4 scheme case and slash", raw: "HTTP://192.168.0.10:8787/", want: "http://192.168.0.10:8787"},
		{name: "HTTP loopback default port", raw: "http://127.0.0.1:80", want: "http://127.0.0.1"},
		{name: "HTTP mDNS nondefault port", raw: "http://raspberrypi.local:8787", want: "http://raspberrypi.local:8787"},
		{name: "HTTP localhost default port", raw: "HTTP://LOCALHOST:80/", want: "http://localhost"},
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

func TestCanonicalRemoteCaptchaOriginErrorAllowsPrivateHTTP(t *testing.T) {
	t.Parallel()
	_, err := netutil.CanonicalRemoteCaptchaOrigin("http://relay.example.com")
	if err == nil {
		t.Fatal("public HTTP origin must still be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "HTTPS origin") || !strings.Contains(msg, "private/local HTTP origin") {
		t.Fatalf("error=%q, want HTTPS or private/local HTTP wording", msg)
	}
	if strings.Contains(msg, "to be an absolute HTTPS origin without") && !strings.Contains(msg, "HTTP") {
		t.Fatalf("error still claims HTTPS-only: %q", msg)
	}
}

func TestValidateRemoteCaptchaBind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		authBaseURL string
		bindAddress string
		wantErr     bool
	}{
		{name: "LAN HTTP with wildcard IPv4", authBaseURL: "http://192.168.0.50:8787", bindAddress: "0.0.0.0"},
		{name: "LAN HTTP with matching IP", authBaseURL: "http://192.168.0.50:8787", bindAddress: "192.168.0.50"},
		{name: "LAN HTTP with wildcard IPv6", authBaseURL: "http://192.168.0.50:8787", bindAddress: "::"},
		{name: "LAN HTTP rejects loopback IPv4", authBaseURL: "http://192.168.0.50:8787", bindAddress: "127.0.0.1", wantErr: true},
		{name: "LAN HTTP rejects IPv6 loopback", authBaseURL: "http://192.168.0.50:8787", bindAddress: "::1", wantErr: true},
		{name: "LAN HTTP rejects localhost", authBaseURL: "http://192.168.0.50:8787", bindAddress: "localhost", wantErr: true},
		{name: "mDNS HTTP rejects loopback", authBaseURL: "http://raspberrypi.local:8787", bindAddress: "127.0.0.1", wantErr: true},
		{name: "mDNS HTTP with wildcard", authBaseURL: "http://raspberrypi.local:8787", bindAddress: "0.0.0.0"},
		{name: "internal DNS with wildcard", authBaseURL: "http://bot.internal:8787", bindAddress: "0.0.0.0"},
		{name: "loopback HTTP allows loopback bind", authBaseURL: "http://127.0.0.1:8787", bindAddress: "127.0.0.1"},
		{name: "localhost HTTP allows localhost bind", authBaseURL: "http://localhost:8787", bindAddress: "localhost"},
		{name: "HTTPS public allows loopback bind", authBaseURL: "https://relay.example.com", bindAddress: "127.0.0.1"},
		{name: "HTTPS public allows localhost bind", authBaseURL: "https://relay.example.com", bindAddress: "localhost"},
		{name: "HTTPS loopback allows loopback bind", authBaseURL: "https://127.0.0.1", bindAddress: "127.0.0.1"},
		{name: "HTTPS LAN IP rejects loopback bind", authBaseURL: "https://192.168.0.50:8443", bindAddress: "127.0.0.1", wantErr: true},
		{name: "link-local HTTP rejects loopback", authBaseURL: "http://169.254.1.1:8787", bindAddress: "127.0.0.1", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := netutil.ValidateRemoteCaptchaBind(test.authBaseURL, test.bindAddress)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ValidateRemoteCaptchaBind(%q, %q) error = nil", test.authBaseURL, test.bindAddress)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateRemoteCaptchaBind(%q, %q): %v", test.authBaseURL, test.bindAddress, err)
			}
		})
	}
}
