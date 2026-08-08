package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Mutation caught: exposing Xvfb over TCP, changing its fixed viewport, or
// dropping its ordering/hardening lets the remote CAPTCHA browser either fail
// to render or create a network-reachable desktop service.
func TestRemoteCaptchaAssetsConfigurePrivateXvfb(t *testing.T) {
	root := repositoryRoot(t)
	display := readDeploymentAsset(t, root, "deploy/valorant-captcha-display.service")
	remoteEnv := readDeploymentAsset(t, root, "deploy/remote-captcha.conf")
	bot := readDeploymentAsset(t, root, "deploy/valorant-bot.service")

	displayUnit := parseSystemdUnit(t, display)
	if got := displayUnit.value("Unit", "Before"); got != "valorant-bot.service" {
		t.Fatalf("display Before=%q, want valorant-bot.service", got)
	}
	if got := displayUnit.value("Service", "User"); got != "valorant" {
		t.Fatalf("display User=%q, want valorant", got)
	}
	if got := displayUnit.value("Service", "Group"); got != "valorant" {
		t.Fatalf("display Group=%q, want valorant", got)
	}
	for key, want := range map[string]string{
		"PrivateTmp":      "true",
		"NoNewPrivileges": "true",
		"ProtectSystem":   "strict",
		"Restart":         "on-failure",
	} {
		if got := displayUnit.value("Service", key); got != want {
			t.Errorf("display %s=%q, want %q", key, got, want)
		}
	}
	execStart := displayUnit.value("Service", "ExecStart")
	for _, want := range []string{"Xvfb", ":99", "-screen 0 1280x900x24", "-nolisten tcp"} {
		if !strings.Contains(execStart, want) {
			t.Errorf("display ExecStart=%q, missing %q", execStart, want)
		}
	}
	for _, forbidden := range []string{"0.0.0.0", "-listen tcp", "--remote-debugging-port"} {
		if strings.Contains(execStart, forbidden) {
			t.Errorf("display ExecStart=%q exposes forbidden listener %q", execStart, forbidden)
		}
	}

	if got := parseEnvironmentFile(t, remoteEnv)["CAPTCHA_BROWSER_MODE"]; got != "remote" {
		t.Fatalf("CAPTCHA_BROWSER_MODE=%q, want remote", got)
	}
	if got := parseEnvironmentFile(t, remoteEnv)["CAPTCHA_DISPLAY"]; got != ":99" {
		t.Fatalf("CAPTCHA_DISPLAY=%q, want :99", got)
	}

	for _, want := range []string{
		"EnvironmentFile=/etc/valorant-bot/env",
		"EnvironmentFile=-/etc/valorant-bot/remote-captcha.conf",
	} {
		if !strings.Contains(bot, want) {
			t.Errorf("bot service does not load %q", want)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readDeploymentAsset(t *testing.T, root, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(contents)
}

type systemdUnit map[string]map[string]string

func parseSystemdUnit(t *testing.T, contents string) systemdUnit {
	t.Helper()
	unit := make(systemdUnit)
	section := ""
	for _, rawLine := range strings.Split(contents, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			if unit[section] == nil {
				unit[section] = make(map[string]string)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			t.Fatalf("invalid systemd line %q", rawLine)
		}
		unit[section][strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return unit
}

func (u systemdUnit) value(section, key string) string {
	return u[section][key]
}

func parseEnvironmentFile(t *testing.T, contents string) map[string]string {
	t.Helper()
	values := make(map[string]string)
	for _, rawLine := range strings.Split(contents, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			t.Fatalf("invalid environment line %q", rawLine)
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values
}
