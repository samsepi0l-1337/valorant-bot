package authweb

import (
	"strings"
	"testing"
)

// Mutation caught: accepting an empty configured display lets remote Chrome
// inherit an arbitrary desktop DISPLAY instead of the Xvfb owned by the bot.
func TestRemoteDisplayCommandRequiresConfiguredDisplay(t *testing.T) {
	_, err := chromeCommandForRemoteDisplay("/opt/chromium", []string{"--incognito"}, " \t")
	if err == nil || !strings.Contains(err.Error(), "CAPTCHA_DISPLAY") {
		t.Fatalf("remote display command error=%v, want missing CAPTCHA_DISPLAY", err)
	}
}

// Mutation caught: omitting the post-allowlist override sends Chromium to the
// local desktop rather than to the configured private Xvfb display.
func TestRemoteDisplayCommandUsesConfiguredDisplayWithoutSecrets(t *testing.T) {
	t.Setenv("DISPLAY", ":42")
	t.Setenv("BOT_SECRET", "remote-display-secret-must-not-reach-chrome")

	cmd, err := chromeCommandForRemoteDisplay("/opt/chromium", []string{"--incognito"}, ":99")
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteDisplayEnvironmentValue(cmd.Env, "DISPLAY"); got != ":99" {
		t.Fatalf("Chrome DISPLAY=%q, want :99", got)
	}
	if got := remoteDisplayEnvironmentValue(cmd.Env, "BOT_SECRET"); got != "" {
		t.Fatalf("Chrome inherited BOT_SECRET=%q", got)
	}
}

// Mutation caught: routing ordinary local launches through the remote display
// override breaks the existing desktop-user Chrome behavior.
func TestRemoteDisplayDoesNotChangeLocalDesktopCommand(t *testing.T) {
	t.Setenv("DISPLAY", ":42")

	cmd, err := chromeCommand("/opt/chromium", []string{"--incognito"})
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteDisplayEnvironmentValue(cmd.Env, "DISPLAY"); got != ":42" {
		t.Fatalf("local Chrome DISPLAY=%q, want inherited desktop :42", got)
	}
}

func remoteDisplayEnvironmentValue(environment []string, key string) string {
	prefix := key + "="
	for i := len(environment) - 1; i >= 0; i-- {
		if strings.HasPrefix(environment[i], prefix) {
			return strings.TrimPrefix(environment[i], prefix)
		}
	}
	return ""
}
