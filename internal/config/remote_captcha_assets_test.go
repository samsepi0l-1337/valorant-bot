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
	installer := readDeploymentAsset(t, root, "deploy/install.sh")
	tmpfiles := readDeploymentAsset(t, root, "deploy/valorant-captcha-display.tmpfiles")

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
	if got := displayUnit.value("Service", "RuntimeDirectory"); got != "" {
		t.Errorf("display RuntimeDirectory=%q; shared X11 socket must be root-owned", got)
	}
	if got := displayUnit.value("Service", "ReadWritePaths"); got != "" {
		t.Errorf("display ReadWritePaths=%q; Xvfb must not write bot state", got)
	}
	const socketPath = "/run/valorant-captcha-display:/tmp/.X11-unix"
	if got := displayUnit.value("Service", "BindPaths"); got != socketPath {
		t.Errorf("display BindPaths=%q, want %q", got, socketPath)
	}
	if got := displayUnit.value("Unit", "After"); got != "systemd-tmpfiles-setup.service" {
		t.Errorf("display After=%q, want systemd-tmpfiles-setup.service", got)
	}
	if !strings.Contains(execStart, "/usr/bin/Xvfb") {
		t.Errorf("display ExecStart=%q, want exact /usr/bin/Xvfb", execStart)
	}

	tmpfileFields := strings.Fields(strings.TrimSpace(tmpfiles))
	if got, want := strings.Join(tmpfileFields, " "), "d /run/valorant-captcha-display 1777 root root -"; got != want {
		t.Errorf("tmpfiles entry=%q, want %q", got, want)
	}

	if got := parseEnvironmentFile(t, remoteEnv)["CAPTCHA_BROWSER_MODE"]; got != "remote" {
		t.Fatalf("CAPTCHA_BROWSER_MODE=%q, want remote", got)
	}
	if got := parseEnvironmentFile(t, remoteEnv)["CAPTCHA_DISPLAY"]; got != ":99" {
		t.Fatalf("CAPTCHA_DISPLAY=%q, want :99", got)
	}

	if !strings.Contains(bot, "EnvironmentFile=/etc/valorant-bot/env") {
		t.Error("bot service does not load its base environment file")
	}
	if strings.Contains(bot, "remote-captcha.conf") {
		t.Error("base bot service loads remote mode settings outside the opt-in drop-in")
	}

	dropIn := generatedRemoteCaptchaDropIn(t, installer)
	dropInUnit := parseSystemdUnit(t, dropIn)
	if got := dropInUnit.value("Unit", "Requires"); got != "valorant-captcha-display.service" {
		t.Errorf("remote drop-in Requires=%q, want display service", got)
	}
	if got := dropInUnit.value("Unit", "After"); got != "valorant-captcha-display.service" {
		t.Errorf("remote drop-in After=%q, want display service", got)
	}
	if got := dropInUnit.value("Service", "EnvironmentFile"); got != "/etc/valorant-bot/remote-captcha.conf" {
		t.Errorf("remote drop-in EnvironmentFile=%q", got)
	}
	if got := dropInUnit.value("Service", "BindPaths"); got != socketPath {
		t.Errorf("remote drop-in BindPaths=%q, want %q", got, socketPath)
	}
}

// Mutation caught: persisting remote mode into the caller-provided base env,
// running apt implicitly, or deleting a shared identity makes an opt-in
// deployment affect unrelated local/QR-only installations.
func TestRemoteCaptchaScriptsKeepOptInStateDeploymentOwned(t *testing.T) {
	root := repositoryRoot(t)
	setup := readDeploymentAsset(t, root, "scripts/setup-pi.sh")
	installer := readDeploymentAsset(t, root, "deploy/install.sh")
	uninstaller := readDeploymentAsset(t, root, "deploy/uninstall.sh")

	if body := shellFunction(t, setup, "write_env_file"); strings.Contains(body, "CAPTCHA_BROWSER_MODE") || strings.Contains(body, "CAPTCHA_DISPLAY") {
		t.Error("setup-pi writes remote CAPTCHA settings into the base env file")
	}
	for _, script := range []string{setup, installer} {
		if !strings.Contains(script, "[[ ! -x /usr/bin/Xvfb ]]") {
			t.Error("remote dependency validation does not match /usr/bin/Xvfb service executable")
		}
		if !strings.Contains(script, "sudo apt-get update && sudo apt-get install -y xvfb chromium") {
			t.Error("missing exact operator apt instruction")
		}
		for _, line := range strings.Split(script, "\n") {
			if strings.Contains(line, "apt-get") && !strings.Contains(line, "echo") {
				t.Errorf("script executes apt command: %q", line)
			}
		}
	}
	for _, want := range []string{
		"rm -f /etc/valorant-bot/remote-captcha.conf",
		"rm -f /etc/systemd/system/valorant-captcha-display.service",
		"rm -f /etc/tmpfiles.d/valorant-captcha-display.conf",
		"rmdir /run/valorant-captcha-display",
	} {
		if !strings.Contains(uninstaller, want) {
			t.Errorf("uninstall does not remove deployment-owned remote asset %q", want)
		}
	}
	normalUninstall, _, ok := strings.Cut(uninstaller, "if [[ \"$PURGE\" -eq 1 ]]; then")
	if !ok {
		t.Fatal("uninstall purge boundary is missing")
	}
	if strings.Contains(normalUninstall, "rm -f /etc/valorant-bot/env") {
		t.Error("normal uninstall removes the retained base environment")
	}
	for _, forbidden := range []string{"userdel valorant", "groupdel valorant", "rm -rf"} {
		if strings.Contains(uninstaller, forbidden) {
			t.Errorf("uninstall contains unsafe cleanup %q", forbidden)
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

func generatedRemoteCaptchaDropIn(t *testing.T, installer string) string {
	t.Helper()
	const start = "cat > \"$REMOTE_CAPTCHA_DROPIN_TMP\" <<'EOF'\n"
	startAt := strings.Index(installer, start)
	if startAt < 0 {
		t.Fatal("remote CAPTCHA drop-in heredoc is missing")
	}
	remaining := installer[startAt+len(start):]
	endAt := strings.Index(remaining, "\nEOF\n")
	if endAt < 0 {
		t.Fatal("remote CAPTCHA drop-in heredoc is unterminated")
	}
	return remaining[:endAt]
}

func shellFunction(t *testing.T, script, name string) string {
	t.Helper()
	start := strings.Index(script, name+"() {")
	if start < 0 {
		t.Fatalf("function %s is missing", name)
	}
	remaining := script[start:]
	end := strings.Index(remaining, "\n}\n")
	if end < 0 {
		t.Fatalf("function %s is unterminated", name)
	}
	return remaining[:end]
}
