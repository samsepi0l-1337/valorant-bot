package valorantbot

import "testing"

func validBindTestConfig() Config {
	return Config{
		DiscordToken: "token",
		DiscordAppID: "app-id",
		BotSecret:    "0123456789abcdef0123456789abcdef",
		AuthBaseURL:  "http://127.0.0.1:8787",
	}
}

// Mutation caught: reverting the runtime default to a wildcard bind exposes
// AUTH_PORT on LAN interfaces even when the operator only configured a tunnel.
func TestNewDefaultsAuthBindAddressToIPv4Loopback(t *testing.T) {
	bot, err := New(validBindTestConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if bot.cfg.AuthBindAddress != "127.0.0.1" {
		t.Fatalf("AuthBindAddress = %q, want 127.0.0.1", bot.cfg.AuthBindAddress)
	}
}

func TestNewAcceptsExplicitAuthBindAddressOverrides(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "IPv4 any", raw: "0.0.0.0", want: "0.0.0.0"},
		{name: "IPv6 loopback", raw: "::1", want: "::1"},
		{name: "IPv6 any", raw: "::", want: "::"},
		{name: "localhost canonicalized", raw: "LOCALHOST", want: "localhost"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := validBindTestConfig()
			cfg.AuthBindAddress = test.raw
			bot, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if bot.cfg.AuthBindAddress != test.want {
				t.Fatalf("AuthBindAddress = %q, want %q", bot.cfg.AuthBindAddress, test.want)
			}
		})
	}
}

func TestNewRejectsInvalidAuthBindAddress(t *testing.T) {
	for _, raw := range []string{
		"https://127.0.0.1",
		"127.0.0.1:8787",
		"[::1]",
		"relay.example.com",
		" 0.0.0.0",
		"localhost\n",
	} {
		t.Run(raw, func(t *testing.T) {
			cfg := validBindTestConfig()
			cfg.AuthBindAddress = raw
			if _, err := New(cfg); err == nil {
				t.Fatalf("New accepted invalid AuthBindAddress %q", raw)
			}
		})
	}
}

func TestNewRejectsLANHTTPRemoteCaptchaWithLoopbackBind(t *testing.T) {
	cfg := validBindTestConfig()
	cfg.CaptchaBrowserMode = "remote"
	cfg.AuthBaseURL = "http://192.168.0.50:8787"
	cfg.AuthBindAddress = "127.0.0.1"
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted LAN HTTP remote captcha with loopback bind")
	}
}

func TestNewAcceptsLANHTTPRemoteCaptchaWithWildcardBind(t *testing.T) {
	cfg := validBindTestConfig()
	cfg.CaptchaBrowserMode = "remote"
	cfg.AuthBaseURL = "HTTP://192.168.0.50:8787/"
	cfg.AuthBindAddress = "0.0.0.0"
	bot, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if bot.cfg.AuthBindAddress != "0.0.0.0" {
		t.Fatalf("AuthBindAddress = %q, want 0.0.0.0", bot.cfg.AuthBindAddress)
	}
	if bot.cfg.AuthBaseURL != "http://192.168.0.50:8787" {
		t.Fatalf("AuthBaseURL = %q, want canonical LAN HTTP origin", bot.cfg.AuthBaseURL)
	}
}

// These literal expectations prove the runtime address builder does not
// concatenate IPv6 and port into the ambiguous "::1:8787" spelling.
func TestAuthListenAddressUsesJoinHostPort(t *testing.T) {
	for _, test := range []struct {
		name string
		host string
		port int
		want string
	}{
		{name: "IPv4 loopback", host: "127.0.0.1", port: 8787, want: "127.0.0.1:8787"},
		{name: "IPv4 any", host: "0.0.0.0", port: 9999, want: "0.0.0.0:9999"},
		{name: "IPv6 loopback", host: "::1", port: 8787, want: "[::1]:8787"},
		{name: "IPv6 any", host: "::", port: 8787, want: "[::]:8787"},
		{name: "localhost", host: "localhost", port: 8787, want: "localhost:8787"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := authListenAddress(test.host, test.port); got != test.want {
				t.Fatalf("authListenAddress(%q, %d) = %q, want %q", test.host, test.port, got, test.want)
			}
		})
	}
}
