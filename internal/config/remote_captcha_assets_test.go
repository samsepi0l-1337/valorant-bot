package config

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	const authPath = "/run/valorant-captcha-display/Xauthority"
	for _, want := range []string{"Xvfb", ":99", "-screen 0 1280x900x24", "-nolisten tcp", "-auth " + authPath} {
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
		t.Errorf("display RuntimeDirectory=%q; tmpfiles owns the shared X11 directory lifecycle", got)
	}
	if got := displayUnit.value("Service", "ReadWritePaths"); got != "" {
		t.Errorf("display ReadWritePaths=%q; Xvfb must not write bot state", got)
	}
	const socketSource = "/run/valorant-captcha-display/X11-unix"
	const displaySocketBind = socketSource + ":/tmp/.X11-unix"
	if got := displayUnit.value("Service", "BindPaths"); got != displaySocketBind {
		t.Errorf("display BindPaths=%q, want %q", got, displaySocketBind)
	}
	if got := displayUnit.value("Unit", "After"); got != "systemd-tmpfiles-setup.service" {
		t.Errorf("display After=%q, want systemd-tmpfiles-setup.service", got)
	}
	if !strings.Contains(execStart, "/usr/bin/Xvfb") {
		t.Errorf("display ExecStart=%q, want exact /usr/bin/Xvfb", execStart)
	}
	if got := displayUnit.value("Service", "ExecStartPre"); got != "/usr/local/libexec/valorant-bot/prepare-captcha-display-auth" {
		t.Errorf("display ExecStartPre=%q, want deployment-owned Xauthority helper", got)
	}
	if got := displayUnit.value("Service", "ExecStopPost"); got != "/usr/bin/rm -f "+authPath {
		t.Errorf("display ExecStopPost=%q, want exact owned Xauthority cleanup", got)
	}

	tmpfileEntries := parseTmpfilesEntries(t, tmpfiles)
	for path, want := range map[string]string{
		"/run/valorant-captcha-display":          "d /run/valorant-captcha-display 0700 valorant valorant -",
		"/run/valorant-captcha-display/X11-unix": "d /run/valorant-captcha-display/X11-unix 01777 root root -",
	} {
		if got := tmpfileEntries[path]; got != want {
			t.Errorf("tmpfiles entry for %s=%q, want %q", path, got, want)
		}
	}

	if got := parseEnvironmentFile(t, remoteEnv)["CAPTCHA_BROWSER_MODE"]; got != "remote" {
		t.Fatalf("CAPTCHA_BROWSER_MODE=%q, want remote", got)
	}
	if got := parseEnvironmentFile(t, remoteEnv)["CAPTCHA_DISPLAY"]; got != ":99" {
		t.Fatalf("CAPTCHA_DISPLAY=%q, want :99", got)
	}
	if got := parseEnvironmentFile(t, remoteEnv)["XAUTHORITY"]; got != authPath {
		t.Fatalf("XAUTHORITY=%q, want %q", got, authPath)
	}

	if !strings.Contains(bot, "EnvironmentFile=/etc/valorant-bot/env") {
		t.Error("bot service does not load its base environment file")
	}
	if strings.Contains(bot, "remote-captcha.conf") {
		t.Error("base bot service loads remote mode settings outside the opt-in drop-in")
	}

	dropIn := generatedRemoteCaptchaDropIn(t, installer)
	dropInUnit := parseSystemdUnit(t, dropIn)
	if got := dropInUnit.value("Unit", "Wants"); got != "valorant-captcha-display.service" {
		t.Errorf("remote drop-in Wants=%q, want display service", got)
	}
	if got := dropInUnit.value("Unit", "Requires"); got != "" {
		t.Errorf("remote drop-in Requires=%q; display failure must not stop QR-capable bot", got)
	}
	if got := dropInUnit.value("Unit", "After"); got != "valorant-captcha-display.service" {
		t.Errorf("remote drop-in After=%q, want display service", got)
	}
	if got := dropInUnit.value("Service", "EnvironmentFile"); got != "/etc/valorant-bot/remote-captcha.conf" {
		t.Errorf("remote drop-in EnvironmentFile=%q", got)
	}
	const optionalBotSocketBind = "-" + displaySocketBind
	if got := dropInUnit.value("Service", "BindPaths"); got != optionalBotSocketBind {
		t.Errorf("remote drop-in BindPaths=%q, want optional %q", got, optionalBotSocketBind)
	} else {
		missingSource := filepath.Join(t.TempDir(), "missing-X11-unix")
		missingBind := strings.Replace(got, socketSource, missingSource, 1)
		if err := validateSystemdBindSource(missingBind); err != nil {
			t.Errorf("optional bot bind rejects absent X11 source: %v", err)
		}
		if err := validateSystemdBindSource(strings.TrimPrefix(missingBind, "-")); err == nil {
			t.Error("required bind unexpectedly accepts absent X11 source")
		}
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
	authHelper := readDeploymentAsset(t, root, "deploy/prepare-captcha-display-auth")

	if body := shellFunction(t, setup, "write_env_file"); strings.Contains(body, "CAPTCHA_BROWSER_MODE=remote") || strings.Contains(body, "CAPTCHA_DISPLAY") {
		t.Error("setup-pi writes remote CAPTCHA settings into the base env file")
	} else if !strings.Contains(body, "CAPTCHA_BROWSER_MODE=disabled") {
		t.Error("setup-pi base env does not explicitly default to QR-only disabled mode")
	}
	for _, script := range []string{setup, installer} {
		if !strings.Contains(script, "[[ ! -x /usr/bin/Xvfb ]]") {
			t.Error("remote dependency validation does not match /usr/bin/Xvfb service executable")
		}
		for _, dependency := range []string{"/usr/bin/xauth", "/usr/bin/mcookie"} {
			if !strings.Contains(script, dependency) {
				t.Errorf("remote dependency validation does not check %s", dependency)
			}
		}
		if !strings.Contains(script, "sudo apt-get update && sudo apt-get install -y xvfb chromium xauth util-linux python3") {
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
		"rm -f /usr/local/libexec/valorant-bot/prepare-captcha-display-auth",
		"rm -f /run/valorant-captcha-display/Xauthority",
		"rmdir /run/valorant-captcha-display/X11-unix",
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
	for _, want := range []string{
		"umask 077",
		"/usr/bin/mcookie",
		"MIT-MAGIC-COOKIE-1",
		"/usr/bin/xauth -f \"$auth_tmp\" source -",
		"mv -f \"$auth_tmp\" \"$AUTH_FILE\"",
	} {
		if !strings.Contains(authHelper, want) {
			t.Errorf("Xauthority helper missing %q", want)
		}
	}
	for _, forbidden := range []string{"echo \"$cookie\"", "set -x", "logger"} {
		if strings.Contains(authHelper, forbidden) {
			t.Errorf("Xauthority helper may expose cookie through %q", forbidden)
		}
	}
}

// Mutation caught: a prefix-only HTTPS check accepts paths, credentials,
// delimiters, or malformed hosts and can mutate a remote installation before
// the application gets a chance to enforce its authoritative runtime check.
func TestRemoteCaptchaDeploymentOriginValidator(t *testing.T) {
	root := repositoryRoot(t)
	validator := filepath.Join(root, "deploy", "validate-remote-captcha-origin.py")

	for _, valid := range []string{
		"https://relay.example.com",
		"https://relay.example.com/",
		"HTTPS://Relay.Example.COM:443/",
		"https://relay.example.com:0443",
		"https://192.0.2.10:8443",
		"https://[2001:0DB8::1]:08443",
		"http://192.168.0.10:8787",
		"http://127.0.0.1:8787",
		"http://raspberrypi.local:8787",
	} {
		t.Run("valid_"+valid, func(t *testing.T) {
			if output, err := exec.Command("python3", validator, valid).CombinedOutput(); err != nil {
				t.Fatalf("validator rejected %q: %v: %s", valid, err, output)
			} else if len(output) != 0 {
				t.Fatalf("validator printed %q for valid origin", output)
			}
		})
	}

	for _, invalid := range []string{
		"",
		"http://relay.example.com",
		"https://",
		"https:///missing-host",
		"https://relay.example.com/path",
		"https://relay.example.com/%2e",
		"https://relay.example.com?query",
		"https://relay.example.com?",
		"https://relay.example.com#fragment",
		"https://relay.example.com#",
		"https://user@relay.example.com",
		"https://relay.example.com ",
		"https://relay.example.com\n",
		"https://relay.example.com\t",
		"https://relay.example.com:0",
		"https://relay.example.com:-1",
		"https://relay.example.com:abc",
		"https://relay.example.com:65536",
		"https://relay.example.com:",
		"https://bad_host.example.com",
		"https://192.168.001.1",
		"https://127.1",
		"https://0x7f.0x0.0x0.0x1",
		"https://[fe80::1%25eth0]",
		"https://[::ffff:192.0.2.1]",
		"https://[::ffff:c000:201]:8443",
	} {
		t.Run("invalid_"+strings.ReplaceAll(invalid, "/", "_"), func(t *testing.T) {
			if output, err := exec.Command("python3", validator, invalid).CombinedOutput(); err == nil {
				t.Fatalf("validator accepted %q", invalid)
			} else if strings.Contains(string(output), invalid) && invalid != "" {
				t.Fatalf("validator echoed rejected input %q", invalid)
			}
		})
	}

	setup := readDeploymentAsset(t, root, "scripts/setup-pi.sh")
	installer := readDeploymentAsset(t, root, "deploy/install.sh")
	for name, script := range map[string]string{"setup-pi": setup, "install": installer} {
		if !strings.Contains(script, "validate-remote-captcha-origin.py") {
			t.Errorf("%s does not use deterministic origin validator", name)
		}
	}
	if got := strings.Count(installer, "validate_remote_captcha_origin"); got != 2 {
		t.Errorf("installer origin validator definition/call count=%d, want 2", got)
	}
	if validateAt, mutateAt := strings.LastIndex(installer, "validate_remote_captcha_origin"), strings.Index(installer, "install -m 0755 \"$BINARY\""); validateAt < 0 || mutateAt < 0 || validateAt > mutateAt {
		t.Errorf("installer origin validation must precede installation mutation (validate=%d mutate=%d)", validateAt, mutateAt)
	}
	hostBranch := setup[strings.Index(setup, "if [[ -n \"$HOST\" ]]"):]
	validateAt := strings.Index(hostBranch, "validate_remote_captcha_origin \"$AUTH_BASE_URL\"")
	for _, mutation := range []string{"make build-pi", "scp -q"} {
		if mutateAt := strings.Index(hostBranch, mutation); validateAt < 0 || mutateAt < 0 || validateAt > mutateAt {
			t.Errorf("setup-pi origin validation must precede remote mutation %q (validate=%d mutate=%d)", mutation, validateAt, mutateAt)
		}
	}
}

// Mutation caught: trusting only the caller's URL lets a remote setup stage a
// binary and deployment tree before discovering that the installed systemd env
// still names a different public origin.
func TestRemoteCaptchaTargetOriginValidatorUsesInstalledEnv(t *testing.T) {
	root := repositoryRoot(t)
	validator := filepath.Join(root, "deploy", "validate-remote-captcha-origin.py")
	caller := "https://caller.example.com"

	t.Run("missing target env accepts validated caller", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing-env")
		if output, err := runTargetOriginValidator(validator, missing, caller); err != nil {
			t.Fatalf("missing target env: %v: %s", err, output)
		}
	})

	t.Run("matching installed origin", func(t *testing.T) {
		envPath := filepath.Join(t.TempDir(), "env")
		if err := os.WriteFile(envPath, []byte("DISCORD_TOKEN=do-not-print\nAUTH_BASE_URL="+caller+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if output, err := runTargetOriginValidator(validator, envPath, caller); err != nil {
			t.Fatalf("matching target env: %v: %s", err, output)
		} else if len(output) != 0 {
			t.Fatalf("matching target validator output=%q, want empty", output)
		}
	})

	for _, test := range []struct {
		name string
		env  string
	}{
		{name: "mismatch", env: "DISCORD_TOKEN=do-not-print\nAUTH_BASE_URL=https://installed.example.com\n"},
		{name: "duplicate", env: "AUTH_BASE_URL=" + caller + "\n  AUTH_BASE_URL=" + caller + "\n"},
		{name: "invalid installed origin", env: "AUTH_BASE_URL=http://installed.example.com\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			envPath := filepath.Join(t.TempDir(), "env")
			if err := os.WriteFile(envPath, []byte(test.env), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := runTargetOriginValidator(validator, envPath, caller)
			if err == nil {
				t.Fatal("target validator accepted conflicting installed env")
			}
			if got, want := string(output), "remote CAPTCHA target preflight failed\n"; got != want {
				t.Fatalf("target failure output=%q, want generic %q", got, want)
			}
			for _, secret := range []string{caller, "https://installed.example.com", "do-not-print"} {
				if strings.Contains(string(output), secret) {
					t.Fatalf("target failure output exposed %q", secret)
				}
			}
		})
	}
}

// Mutation caught: moving target-env validation after make/scp causes local
// build output or remote staging writes even though the installed origin and
// caller origin disagree.
func TestSetupPiRemoteTargetMismatchHasZeroBuildOrCopyMutations(t *testing.T) {
	root := repositoryRoot(t)
	fakeBin := t.TempDir()
	mutationLog := filepath.Join(t.TempDir(), "mutations")
	preflightLog := filepath.Join(t.TempDir(), "preflight")
	targetEnv := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(targetEnv, []byte("DISCORD_TOKEN=do-not-print\nAUTH_BASE_URL=https://installed.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "make"), "#!/bin/sh\nprintf 'make\\n' >> \"$MUTATION_LOG\"\n")
	writeExecutable(t, filepath.Join(fakeBin, "scp"), "#!/bin/sh\nprintf 'scp\\n' >> \"$MUTATION_LOG\"\n")
	writeExecutable(t, filepath.Join(fakeBin, "ssh"), `#!/bin/sh
case "$*" in
  *"/usr/bin/python3"*"/etc/valorant-bot/env"*)
    printf '%s\n' "$*" > "$PREFLIGHT_LOG"
    case " $* " in
      *" -t "*|*" -tt "*) ;;
      *) exit 86 ;;
    esac
    remote_command="$3"
    remote_command="${remote_command#sudo }"
    remote_command="$(printf '%s' "$remote_command" | sed "s#/etc/valorant-bot/env#$FAKE_TARGET_ENV#g")"
    exec /bin/sh -c "$remote_command"
    ;;
  *) exit 0 ;;
esac
`)

	caller := "https://caller.example.com"
	cmd := exec.Command(filepath.Join(root, "scripts", "setup-pi.sh"),
		"--host", "pi@example.test", "--remote-captcha", "--yes", "--skip-start")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"MUTATION_LOG="+mutationLog,
		"PREFLIGHT_LOG="+preflightLog,
		"FAKE_TARGET_ENV="+targetEnv,
		"DISCORD_TOKEN=test-token",
		"DISCORD_APP_ID=test-app",
		"BOT_SECRET=01234567890123456789012345678901",
		"AUTH_BASE_URL="+caller,
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("setup-pi accepted target AUTH_BASE_URL mismatch")
	}
	if data, readErr := os.ReadFile(mutationLog); readErr == nil && len(data) != 0 {
		t.Fatalf("setup-pi mutated build/copy state before target validation: %q", data)
	} else if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if strings.Contains(string(output), caller) {
		t.Fatalf("setup-pi mismatch output exposed caller origin: %s", output)
	}
	if !strings.Contains(string(output), "remote CAPTCHA target preflight failed") {
		t.Fatalf("setup-pi mismatch output=%q, want generic target preflight failure", output)
	}
	preflight, readErr := os.ReadFile(preflightLog)
	if readErr != nil {
		t.Fatalf("read remote preflight invocation: %v", readErr)
	}
	invocation := string(preflight)
	if !strings.Contains(" "+invocation+" ", " -tt ") {
		t.Fatalf("remote preflight invocation did not force a sudo-capable TTY: %q", invocation)
	}
	if !strings.Contains(invocation, "sudo /usr/bin/python3 -c") {
		t.Fatalf("remote preflight invocation=%q, want sudo python -c bootstrap", invocation)
	}
	if !strings.Contains(invocation, "base64.b64decode") {
		t.Fatalf("remote preflight invocation=%q, want encoded validator bootstrap", invocation)
	}
	for _, forbidden := range []string{caller, "/usr/bin/python3 - --target-env"} {
		if strings.Contains(invocation, forbidden) {
			t.Fatalf("remote preflight invocation exposed unsafe transport %q: %q", forbidden, invocation)
		}
	}
}

// Mutation caught: a normal `read` leaves terminal echo enabled and writes the
// Discord token into the terminal transcript. The secret prompt must disable
// echo only while reading, restore it afterwards, and print its own newline.
func TestSetupPiSecretPromptDisablesEchoAndRestoresTerminal(t *testing.T) {
	root := repositoryRoot(t)
	setup := readDeploymentAsset(t, root, "scripts/setup-pi.sh")
	probe := runSecretPromptPTYProbe(t, shellFunction(t, setup, "prompt"))
	if probe.ExitCode != 0 {
		t.Fatalf("secret prompt exit code=%d, want 0", probe.ExitCode)
	}
	if !probe.EchoBefore {
		t.Fatal("PTY fixture did not begin with echo enabled")
	}
	if probe.EchoDuring {
		t.Fatal("Discord token prompt left terminal echo enabled while reading")
	}
	if !probe.EchoAfter {
		t.Fatal("Discord token prompt did not restore terminal echo")
	}
	if probe.SecretLeaked {
		t.Fatal("Discord token prompt echoed the secret into its transcript")
	}
	if !probe.NewlineAfterPrompt {
		t.Fatal("secret prompt did not print a newline after the hidden input")
	}
	if !probe.ValueAccepted {
		t.Fatal("secret prompt did not export the complete hidden value")
	}
}

// Mutation caught: trying a hidden read from a pipe can consume a secret or
// hang without a controlling terminal. Missing secrets must fail before read.
func TestSetupPiSecretPromptRejectsNonTTYBeforeReading(t *testing.T) {
	root := repositoryRoot(t)
	setup := readDeploymentAsset(t, root, "scripts/setup-pi.sh")
	secret := "discord-token-must-not-appear"
	harness := filepath.Join(t.TempDir(), "prompt.sh")
	writeExecutable(t, harness, "#!/usr/bin/env bash\nset -euo pipefail\nYES=0\n"+
		shellFunction(t, setup, "prompt")+"\n}\nprompt DISCORD_TOKEN 'Discord bot token (DISCORD_TOKEN)' secret\n")
	cmd := exec.Command(harness)
	cmd.Stdin = strings.NewReader(secret + "\n")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("secret prompt accepted a non-TTY input stream")
	}
	if !strings.Contains(string(output), "stdin is not a TTY") {
		t.Fatal("non-TTY prompt failure did not explain the terminal requirement")
	}
	if bytes.Contains(output, []byte(secret)) {
		t.Fatal("non-TTY prompt failure exposed the unread secret")
	}
}

// Mutations caught: a fixed remote env filename can collide with another
// setup, and cleanup placed only after install leaks staged secrets when sudo
// install fails. Both successful and failed installs must clean randomized
// local and remote task paths without exposing credentials.
func TestSetupPiRemoteInstallAlwaysCleansRandomizedSecretTemps(t *testing.T) {
	root := repositoryRoot(t)
	for _, test := range []struct {
		name        string
		installExit int
		remotePath  string
	}{
		{name: "success", installExit: 0, remotePath: "/tmp/valorant-bot-install.A1b2C3"},
		{name: "sudo install failure", installExit: 29, remotePath: "/tmp/valorant-bot-install.Z9y8X7"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := runRemoteSetupTransfer(t, root, test.remotePath, test.installExit, false, "")
			if test.installExit == 0 && result.err != nil {
				t.Fatalf("remote setup success path failed: %v", result.err)
			}
			if test.installExit != 0 && result.err == nil {
				t.Fatal("remote setup ignored sudo install failure")
			}
			assertSetupSecretsAbsent(t, result)
			assertRandomizedSetupPathsAndCleanup(t, result, test.remotePath, true)
		})
	}
}

// Mutation caught: removing the local temp only after scp leaks the env file
// when the copy itself fails, before sudo install is attempted.
func TestSetupPiRemoteEnvCopyFailureCleansLocalSecretTemp(t *testing.T) {
	root := repositoryRoot(t)
	remotePath := "/tmp/valorant-bot-install.C4d5E6"
	result := runRemoteSetupTransfer(t, root, remotePath, 0, true, "")
	if result.err == nil {
		t.Fatal("remote setup ignored env scp failure")
	}
	assertSetupSecretsAbsent(t, result)
	assertRandomizedSetupPathsAndCleanup(t, result, remotePath, false)
	if strings.Contains(result.sshLog, "sudo ./deploy/install.sh") {
		t.Fatal("remote install ran after env scp failure")
	}
}

// Mutations caught: replacing mktemp with one fixed task basename lets
// separate setup invocations collide, while widening chmod exposes the env to
// other local users. Observe both paths and modes while the files still exist.
func TestSetupPiLocalTaskPathsAreUniqueAndPrivate(t *testing.T) {
	root := repositoryRoot(t)
	sharedTempRoot := filepath.Join(t.TempDir(), "shared-tmp")
	var paths []string
	for index, remotePath := range []string{
		"/tmp/valorant-bot-install.F1a2B3",
		"/tmp/valorant-bot-install.G4c5D6",
	} {
		result := runRemoteSetupTransfer(t, root, remotePath, 0, false, sharedTempRoot)
		if result.err != nil {
			t.Fatalf("setup run %d failed: %v", index+1, result.err)
		}
		assertSetupSecretsAbsent(t, result)
		assertRandomizedSetupPathsAndCleanup(t, result, remotePath, true)
		paths = append(paths, result.localEnvPath)
	}
	if paths[0] == paths[1] {
		t.Fatal("two setup runs sharing TMPDIR reused the same local task path")
	}
}

// Mutation caught: local-Pi setup has no statement after a failed installer
// that can remove its secret env. EXIT ownership must remove the task temp.
func TestSetupPiLocalInstallFailureCleansSecretTemp(t *testing.T) {
	root := repositoryRoot(t)
	result := runLocalPiSetupFailure(t, readDeploymentAsset(t, root, "scripts/setup-pi.sh"))
	if result.err == nil {
		t.Fatal("local Pi setup ignored installer failure")
	}
	for _, contents := range []string{result.output, result.installLog} {
		for _, secret := range result.secrets {
			if strings.Contains(contents, secret) {
				t.Fatal("local Pi setup exposed a credential in output or command logs")
			}
		}
	}
	if result.localEnvPath == "" {
		t.Fatal("fake local installer did not receive an env path")
	}
	assertTaskScopedLocalEnvRemoved(t, result.localTempRoot, result.localEnvPath, result.localDirMode, result.localEnvMode)
}

// Mutations caught: `start` and `enable --now` leave already-active services
// on their old process image after daemon-reload, while `try-restart` leaves an
// inactive service stopped. A reinstall must apply the newly installed assets
// in both initial states.
func TestInstallRestartAppliesReloadedAssetsToActiveAndInactiveServices(t *testing.T) {
	root := repositoryRoot(t)
	installer := readDeploymentAsset(t, root, "deploy/install.sh")
	wantCalls := strings.Join([]string{
		"daemon-reload",
		"enable valorant-captcha-display",
		"restart valorant-captcha-display",
		"enable valorant-bot",
		"restart valorant-bot",
		"--no-pager --full status valorant-bot",
	}, "\n") + "\n"

	for _, initiallyActive := range []bool{false, true} {
		name := "inactive"
		if initiallyActive {
			name = "active"
		}
		t.Run(name, func(t *testing.T) {
			result := runInstallServiceActivation(t, installer, initiallyActive, false)
			if result.err != nil {
				t.Fatalf("installer activation failed: %v: %s", result.err, result.output)
			}
			if result.calls != wantCalls {
				t.Fatalf("systemctl calls=\n%s\nwant=\n%s", result.calls, wantCalls)
			}
			for unit, generation := range map[string]string{
				"display": result.displayGeneration,
				"bot":     result.botGeneration,
			} {
				if generation != "new" {
					t.Errorf("%s process generation=%q, want newly loaded assets", unit, generation)
				}
			}
		})
	}
}

// Mutation caught: allowing display restart failure to trip `set -e`, or
// coupling the bot to the display restart, takes the QR-capable bot down during
// a remote CAPTCHA reinstall.
func TestInstallDisplayRestartFailureStillRestartsQRBot(t *testing.T) {
	root := repositoryRoot(t)
	installer := readDeploymentAsset(t, root, "deploy/install.sh")
	result := runInstallServiceActivation(t, installer, true, true)
	if result.err != nil {
		t.Fatalf("display restart failure aborted installer: %v: %s", result.err, result.output)
	}
	if !strings.Contains(result.output, "warning: remote CAPTCHA display failed; starting the bot with Riot Mobile QR still available") {
		t.Fatalf("installer output=%q, want QR-survival warning", result.output)
	}
	if result.displayGeneration != "old" {
		t.Fatalf("failed display restart generation=%q, want old process retained by fake", result.displayGeneration)
	}
	if result.botGeneration != "new" {
		t.Fatalf("bot process generation=%q, want restart after display failure", result.botGeneration)
	}
	wantCalls := strings.Join([]string{
		"daemon-reload",
		"enable valorant-captcha-display",
		"restart valorant-captcha-display",
		"enable valorant-bot",
		"restart valorant-bot",
		"--no-pager --full status valorant-bot",
	}, "\n") + "\n"
	if result.calls != wantCalls {
		t.Fatalf("systemctl calls=\n%s\nwant=\n%s", result.calls, wantCalls)
	}
}

// Mutation caught: forwarding a rewritten Host, omitting websocket headers on
// either TLS path, or binding the auth listener broadly breaks the public
// remote viewer while exposing its private upstream unnecessarily.
func TestRemoteCaptchaProxyAndPiAssetsKeepRemoteRelayPrivate(t *testing.T) {
	root := repositoryRoot(t)
	nginx := readDeploymentAsset(t, root, "deploy/nginx.example.conf")
	piTunnel := readDeploymentAsset(t, root, "scripts/pi-tunnel.sh")
	piSetup := readDeploymentAsset(t, root, "scripts/setup-pi.sh")
	piEnv := readDeploymentAsset(t, root, "deploy/env.pi.example")

	if !strings.Contains(nginx, "map $http_upgrade $connection_upgrade") {
		t.Error("nginx does not derive a Connection header for websocket upgrades")
	}
	if got := strings.Count(nginx, "location /"); got != 2 {
		t.Fatalf("nginx location count=%d, want HTTP and TLS locations", got)
	}
	locations := strings.Split(nginx, "location /")[1:]
	for index, location := range locations {
		endMarker := "\n    }"
		if index == 1 {
			endMarker = "\n#     }"
		}
		if endAt := strings.Index(location, endMarker); endAt >= 0 {
			location = location[:endAt]
		} else {
			t.Fatalf("nginx location %d is unterminated", index+1)
		}
		for _, want := range []string{
			"proxy_http_version 1.1;",
			"proxy_set_header Host $http_host;",
			"proxy_set_header Upgrade $http_upgrade;",
			"proxy_set_header Connection $connection_upgrade;",
		} {
			if !strings.Contains(location, want) {
				t.Errorf("nginx location missing %q", want)
			}
		}
	}

	if got := parseEnvironmentFile(t, piEnv)["CAPTCHA_BROWSER_MODE"]; got != "disabled" {
		t.Errorf("Pi template CAPTCHA_BROWSER_MODE=%q, want disabled", got)
	}
	for _, key := range []string{"CAPTCHA_DISPLAY", "XAUTHORITY"} {
		if got := parseEnvironmentFile(t, piEnv)[key]; got != "" {
			t.Errorf("Pi base template %s=%q; remote drop-in must own it", key, got)
		}
	}
	if got := parseEnvironmentFile(t, piEnv)["AUTH_BIND_ADDRESS"]; got != "127.0.0.1" {
		t.Errorf("Pi template AUTH_BIND_ADDRESS=%q, want 127.0.0.1", got)
	}
	baseEnv := shellFunction(t, piSetup, "write_env_file")
	for _, want := range []string{
		"AUTH_BIND_ADDRESS=${AUTH_BIND_ADDRESS:-127.0.0.1}",
		"CAPTCHA_BROWSER_MODE=disabled",
	} {
		if !strings.Contains(baseEnv, want) {
			t.Errorf("Pi base-env generator missing %q", want)
		}
	}
	for _, want := range []string{
		"CAPTCHA_BROWSER_MODE",
		"stable public HTTPS AUTH_BASE_URL",
		"quick tunnel is test-only",
		"WebSocket",
	} {
		if !strings.Contains(piTunnel, want) {
			t.Errorf("Pi tunnel output does not distinguish remote relay requirement %q", want)
		}
	}
}

// Mutation caught: accepting a trycloudflare URL with a path, query, userinfo,
// or mixed origins lets Discord mint viewer cookies against a hostname the
// running bot never advertised.
func TestExtractTrycloudflareOriginFromCloudflaredLog(t *testing.T) {
	const origin = "https://alpha-beta-gamma.trycloudflare.com"
	banner := strings.Join([]string{
		"2026-08-13T08:19:00Z INF Requesting new quick Tunnel on trycloudflare.com...",
		"2026-08-13T08:19:02Z INF +--------------------------------------------------------------------------------------------+",
		"2026-08-13T08:19:02Z INF |  Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):  |",
		"2026-08-13T08:19:02Z INF |  " + origin + "                                               |",
		"2026-08-13T08:19:02Z INF +--------------------------------------------------------------------------------------------+",
		"",
	}, "\n")

	stdout, stderr, err := runTrycloudflareExtractor(t, banner)
	if err != nil {
		t.Fatalf("extractor rejected cloudflared banner: %v: stdout=%q stderr=%q", err, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("extractor stderr=%q, want empty", stderr)
	}
	if stdout != origin+"\n" {
		t.Fatalf("extractor stdout=%q, want %q", stdout, origin+"\n")
	}

	duplicate := banner + "also " + origin + "/\n"
	stdout, stderr, err = runTrycloudflareExtractor(t, duplicate)
	if err != nil {
		t.Fatalf("extractor rejected duplicate canonical origin: %v: stdout=%q stderr=%q", err, stdout, stderr)
	}
	if stdout != origin+"\n" {
		t.Fatalf("duplicate stdout=%q, want canonical %q", stdout, origin+"\n")
	}

	for _, name := range []string{"empty", "not yet"} {
		input := map[string]string{
			"empty":   "",
			"not yet": "INF Starting tunnel\nINF Waiting for connection\n",
		}[name]
		stdout, stderr, err = runTrycloudflareExtractor(t, input)
		if err == nil {
			t.Fatalf("%s: extractor accepted incomplete log: stdout=%q", name, stdout)
		}
		if stdout != "" {
			t.Fatalf("%s: extractor stdout=%q, want empty", name, stdout)
		}
		if strings.Contains(stderr, "https://") || strings.Contains(stderr, "trycloudflare") && strings.Contains(stderr, "alpha-beta") {
			t.Fatalf("%s: extractor echoed input in stderr=%q", name, stderr)
		}
	}

	for _, invalid := range []string{
		"https://alpha-beta-gamma.trycloudflare.com/captcha",
		"https://alpha-beta-gamma.trycloudflare.com?next=1",
		"https://user@alpha-beta-gamma.trycloudflare.com",
		"http://alpha-beta-gamma.trycloudflare.com",
		"https://trycloudflare.com",
		"https://alpha-beta-gamma.trycloudflare.com\nhttps://other-words.trycloudflare.com\n",
	} {
		t.Run("invalid_"+strings.ReplaceAll(invalid, "/", "_"), func(t *testing.T) {
			stdout, stderr, err := runTrycloudflareExtractor(t, invalid)
			if err == nil {
				t.Fatalf("extractor accepted %q", invalid)
			}
			if stdout != "" {
				t.Fatalf("extractor stdout=%q, want empty", stdout)
			}
			for _, leaked := range []string{"alpha-beta-gamma", "other-words", "user@"} {
				if strings.Contains(stderr, leaked) {
					t.Fatalf("extractor echoed %q in stderr=%q", leaked, stderr)
				}
			}
		})
	}
}

// Mutation caught: rewriting the whole .env, using sed, or echoing Discord
// secrets while persisting a quick-tunnel origin leaves credentials in the
// shell history and can drop BOT_SECRET/DISCORD_TOKEN.
func TestWriteRemoteCaptchaEnvPersistsTunnelOriginWithoutRewritingSecrets(t *testing.T) {
	const origin = "https://alpha-beta-gamma.trycloudflare.com"
	const token = "do-not-print"
	initial := strings.Join([]string{
		"DISCORD_TOKEN=" + token,
		"DISCORD_APP_ID=app-id",
		"BOT_SECRET=super-secret-bot-key-32chars!!",
		"AUTH_BASE_URL=http://192.168.0.127:8787",
		"AUTH_PORT=8787",
		"AUTH_BIND_ADDRESS=0.0.0.0",
		"CAPTCHA_BROWSER_MODE=local",
		"# AUTH_BASE_URL=http://<lan-ip>:8787",
		"DATABASE_PATH=./data/bot.db",
		"",
	}, "\n")
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runWriteRemoteCaptchaEnv(t, envPath, origin)
	if err != nil {
		t.Fatalf("env writer rejected valid origin: %v: stdout=%q stderr=%q", err, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("env writer stdout=%q, want empty", stdout)
	}
	if strings.Contains(stderr, token) || strings.Contains(stderr, "super-secret") {
		t.Fatalf("env writer leaked a secret in stderr=%q", stderr)
	}

	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(got)
	parsed := parseEnvironmentFile(t, contents)
	if parsed["DISCORD_TOKEN"] != token || parsed["BOT_SECRET"] != "super-secret-bot-key-32chars!!" {
		t.Fatalf("env writer mutated secrets: %v", parsed)
	}
	if parsed["AUTH_BASE_URL"] != origin {
		t.Fatalf("AUTH_BASE_URL=%q, want %q", parsed["AUTH_BASE_URL"], origin)
	}
	if parsed["AUTH_BIND_ADDRESS"] != "127.0.0.1" {
		t.Fatalf("AUTH_BIND_ADDRESS=%q, want 127.0.0.1", parsed["AUTH_BIND_ADDRESS"])
	}
	if parsed["CAPTCHA_BROWSER_MODE"] != "remote" {
		t.Fatalf("CAPTCHA_BROWSER_MODE=%q, want remote", parsed["CAPTCHA_BROWSER_MODE"])
	}
	if parsed["AUTH_PORT"] != "8787" || parsed["DATABASE_PATH"] != "./data/bot.db" {
		t.Fatalf("env writer dropped unrelated keys: %v", parsed)
	}
	if !strings.Contains(contents, "# AUTH_BASE_URL=http://<lan-ip>:8787") {
		t.Fatal("env writer removed the commented LAN AUTH_BASE_URL example")
	}
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("env mode=%o, want 0600", info.Mode().Perm())
	}

	beforeInvalid := string(got)
	stdout, stderr, err = runWriteRemoteCaptchaEnv(t, envPath, "http://relay.example.com")
	if err == nil {
		t.Fatal("env writer accepted a public HTTP origin")
	}
	if stdout != "" || strings.Contains(stderr, token) || strings.Contains(stderr, origin) {
		t.Fatalf("invalid-origin output leaked state stdout=%q stderr=%q", stdout, stderr)
	}
	afterInvalid, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterInvalid) != beforeInvalid {
		t.Fatal("invalid origin mutated .env")
	}

	dupPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(dupPath, []byte("AUTH_BASE_URL="+origin+"\nAUTH_BASE_URL="+origin+"\nDISCORD_TOKEN="+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeDup, err := os.ReadFile(dupPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = runWriteRemoteCaptchaEnv(t, dupPath, origin)
	if err == nil {
		t.Fatal("env writer accepted duplicate AUTH_BASE_URL keys")
	}
	if stdout != "" || strings.Contains(stderr, token) {
		t.Fatalf("duplicate-key output leaked secrets stdout=%q stderr=%q", stdout, stderr)
	}
	afterDup, err := os.ReadFile(dupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterDup) != string(beforeDup) {
		t.Fatal("duplicate AUTH_BASE_URL mutated .env")
	}

	missing := filepath.Join(t.TempDir(), "missing.env")
	if _, stderr, err = runWriteRemoteCaptchaEnv(t, missing, origin); err == nil {
		t.Fatal("env writer created a missing env file")
	} else if strings.Contains(stderr, origin) {
		t.Fatalf("missing-env stderr echoed origin: %q", stderr)
	}

	appendPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(appendPath, []byte("DISCORD_TOKEN="+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runWriteRemoteCaptchaEnv(t, appendPath, origin); err != nil {
		t.Fatalf("env writer did not append missing remote keys: %v: %s", err, stderr)
	}
	appended := parseEnvironmentFile(t, string(mustReadFile(t, appendPath)))
	if appended["AUTH_BASE_URL"] != origin || appended["AUTH_BIND_ADDRESS"] != "127.0.0.1" || appended["CAPTCHA_BROWSER_MODE"] != "remote" {
		t.Fatalf("appended remote keys=%v", appended)
	}
	if appended["DISCORD_TOKEN"] != token {
		t.Fatal("append path mutated DISCORD_TOKEN")
	}
}

// Mutation caught: starting the bot before a quick-tunnel origin exists, or
// leaving AUTH_BIND_ADDRESS on a LAN bind, publishes Discord links that LTE
// phones cannot open (or exposes AUTH_PORT on the LAN while the tunnel is the
// intended public path).
func TestRunLocalRemoteStartsTunnelBeforeBot(t *testing.T) {
	root := repositoryRoot(t)
	script := readDeploymentAsset(t, root, "scripts/run-local-remote.sh")
	extractor := readDeploymentAsset(t, root, "deploy/extract-trycloudflare-origin.py")

	if !strings.HasPrefix(script, "#!/usr/bin/env bash\n") {
		t.Fatal("run-local-remote.sh is not a bash script")
	}
	for _, want := range []string{
		"set -euo pipefail",
		"scripts/load-dotenv.sh",
		"load_dotenv",
		`cloudflared tunnel --url "http://127.0.0.1:${PORT}"`,
		"extract-trycloudflare-origin.py",
		"validate-remote-captcha-origin.py",
		"write-remote-captcha-env.py",
		`AUTH_BASE_URL="$ORIGIN"`,
		"AUTH_BIND_ADDRESS=127.0.0.1",
		"CAPTCHA_BROWSER_MODE=remote",
		"make build",
		"quick tunnel",
		"trycloudflare",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("run-local-remote.sh missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"set -x",
		">> .env",
		">>\".env\"",
		"sed -i",
		"AUTH_BIND_ADDRESS=0.0.0.0",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("run-local-remote.sh contains forbidden %q", forbidden)
		}
	}

	sourceAt := strings.Index(script, "load_dotenv")
	bindAt := strings.Index(script, "AUTH_BIND_ADDRESS=127.0.0.1")
	tunnelAt := strings.Index(script, `cloudflared tunnel --url "http://127.0.0.1:${PORT}"`)
	extractAt := strings.Index(script, "extract-trycloudflare-origin.py")
	validateAt := strings.Index(script, "validate-remote-captcha-origin.py")
	writeAt := strings.Index(script, "write-remote-captcha-env.py")
	originAt := strings.Index(script, `AUTH_BASE_URL="$ORIGIN"`)
	buildAt := strings.Index(script, "make build")
	botAt := strings.Index(script, "go run ./cmd/bot")
	if sourceAt < 0 || bindAt < 0 || tunnelAt < 0 || extractAt < 0 || validateAt < 0 || writeAt < 0 || originAt < 0 || buildAt < 0 || botAt < 0 {
		t.Fatal("run-local-remote.sh is missing a required staging step")
	}
	if !(sourceAt < bindAt && bindAt < tunnelAt && tunnelAt < extractAt && extractAt < validateAt && validateAt < writeAt && writeAt < originAt && originAt < buildAt && buildAt < botAt) {
		t.Fatalf("run-local-remote.sh stage order source=%d bind=%d tunnel=%d extract=%d validate=%d write=%d origin=%d build=%d bot=%d", sourceAt, bindAt, tunnelAt, extractAt, validateAt, writeAt, originAt, buildAt, botAt)
	}
	if strings.Index(script, "trap") < 0 || strings.Index(script, "kill") < 0 {
		t.Error("run-local-remote.sh does not clean up cloudflared")
	}
	if strings.Contains(script, "exec cloudflared") {
		t.Error("run-local-remote.sh must not exec cloudflared (bot never starts)")
	}
	if strings.Contains(extractor, "sys.argv[1]") && !strings.Contains(extractor, "sys.stdin") {
		t.Error("extractor must read cloudflared logs from stdin")
	}
	if strings.Contains(script, "source .env") {
		t.Error("run-local-remote.sh must not bash-source .env (unquoted cron runs as a command)")
	}

	for name, want := range map[string]string{
		"README.md":                      "scripts/run-local-remote.sh",
		"deploy/lan-remote-captcha.md":   "scripts/run-local-remote.sh",
		"deploy/pi-cloudflare-tunnel.md": "scripts/run-local-remote.sh",
		"deploy/env.local.example":       "scripts/run-local-remote.sh",
	} {
		if !strings.Contains(readDeploymentAsset(t, root, name), want) {
			t.Errorf("%s does not document %s", name, want)
		}
	}
}

// Mutation caught: bash `source .env` treats STORE_RESET_CRON=0 0 * * * as
// STORE_RESET_CRON=0 then runs command `0`. Local examples must quote the
// cron, and the loader must accept the unquoted form already in existing .env
// files (godotenv accepts it; systemd EnvironmentFile takes the rest of the line).
func TestLocalDotenvCronIsSafeForBash(t *testing.T) {
	root := repositoryRoot(t)
	quoted := `STORE_RESET_CRON="0 0 * * *"`
	for _, name := range []string{
		"deploy/env.local.example",
		".env.example",
		".env.local.example",
	} {
		body := readDeploymentAsset(t, root, name)
		if !strings.Contains(body, quoted) {
			t.Errorf("%s missing quoted %s", name, quoted)
		}
		if strings.Contains(body, "STORE_RESET_CRON=0 0 * * *") {
			t.Errorf("%s has unquoted STORE_RESET_CRON", name)
		}
	}

	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("AUTH_PORT=8787\nSTORE_RESET_CRON=0 0 * * *\nCAPTCHA_DISPLAY=:99\nQUOTED=\"hello world\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "set -euo pipefail\nsource " + strconv.Quote(filepath.Join(root, "scripts/load-dotenv.sh")) +
		"\nload_dotenv " + strconv.Quote(envPath) + "\n" +
		"printf 'cron=%s\\n' \"$STORE_RESET_CRON\"\n" +
		"printf 'port=%s\\n' \"$AUTH_PORT\"\n" +
		"printf 'display=%s\\n' \"$CAPTCHA_DISPLAY\"\n" +
		"printf 'quoted=%s\\n' \"$QUOTED\"\n"
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("load_dotenv failed: %v: %s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"cron=0 0 * * *",
		"port=8787",
		"display=:99",
		"quoted=hello world",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
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

func parseTmpfilesEntries(t *testing.T, contents string) map[string]string {
	t.Helper()
	entries := make(map[string]string)
	for _, rawLine := range strings.Split(contents, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 6 {
			t.Fatalf("invalid tmpfiles entry %q", rawLine)
		}
		entries[fields[1]] = strings.Join(fields, " ")
	}
	return entries
}

func validateSystemdBindSource(binding string) error {
	optional := strings.HasPrefix(binding, "-")
	binding = strings.TrimPrefix(binding, "-")
	source, _, ok := strings.Cut(binding, ":")
	if !ok || source == "" {
		return os.ErrInvalid
	}
	_, err := os.Stat(source)
	if err != nil && optional && os.IsNotExist(err) {
		return nil
	}
	return err
}

func runTrycloudflareExtractor(t *testing.T, input string) (string, string, error) {
	t.Helper()
	extractor := filepath.Join(repositoryRoot(t), "deploy", "extract-trycloudflare-origin.py")
	cmd := exec.Command("python3", extractor)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func runWriteRemoteCaptchaEnv(t *testing.T, envPath, origin string) (string, string, error) {
	t.Helper()
	writer := filepath.Join(repositoryRoot(t), "deploy", "write-remote-captcha-env.py")
	cmd := exec.Command("python3", writer, envPath, origin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func runTargetOriginValidator(validator, envPath, caller string) ([]byte, error) {
	source, err := os.ReadFile(validator)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("python3", "-", "--target-env", envPath, caller)
	cmd.Stdin = bytes.NewReader(source)
	return cmd.CombinedOutput()
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

type installActivationResult struct {
	output            string
	calls             string
	displayGeneration string
	botGeneration     string
	err               error
}

type secretPromptProbe struct {
	EchoBefore         bool `json:"echo_before"`
	EchoDuring         bool `json:"echo_during"`
	EchoAfter          bool `json:"echo_after"`
	SecretLeaked       bool `json:"secret_leaked"`
	NewlineAfterPrompt bool `json:"newline_after_prompt"`
	ValueAccepted      bool `json:"value_accepted"`
	ExitCode           int  `json:"exit_code"`
}

func runSecretPromptPTYProbe(t *testing.T, promptFunction string) secretPromptProbe {
	t.Helper()
	testRoot := t.TempDir()
	harness := filepath.Join(testRoot, "prompt.sh")
	writeExecutable(t, harness, "#!/usr/bin/env bash\nset -euo pipefail\nYES=0\n"+promptFunction+"\n}\n"+
		"\nprompt DISCORD_TOKEN 'Discord bot token (DISCORD_TOKEN)' secret\nprintf 'VALUE_LENGTH=%s\\n' \"${#DISCORD_TOKEN}\"\n")
	controller := filepath.Join(testRoot, "pty_probe.py")
	controllerSource := `import json
import os
import pty
import select
import subprocess
import sys
import termios
import time

secret = sys.stdin.buffer.readline().rstrip(b"\n")
master, slave = pty.openpty()

def echo_enabled():
    return bool(termios.tcgetattr(slave)[3] & termios.ECHO)

before = echo_enabled()
process = subprocess.Popen([sys.argv[1]], stdin=slave, stdout=slave, stderr=slave, close_fds=True)
transcript = bytearray()
prompt = b"Discord bot token (DISCORD_TOKEN): "
deadline = time.monotonic() + 5
while prompt not in transcript and time.monotonic() < deadline:
    ready, _, _ = select.select([master], [], [], 0.1)
    if ready:
        transcript.extend(os.read(master, 4096))
if prompt not in transcript:
    process.kill()
    process.wait()
    raise SystemExit("prompt timeout")

during = echo_enabled()
os.write(master, secret + b"\n")
try:
    exit_code = process.wait(timeout=5)
except subprocess.TimeoutExpired:
    process.kill()
    process.wait()
    raise SystemExit("prompt completion timeout")

while True:
    ready, _, _ = select.select([master], [], [], 0.05)
    if not ready:
        break
    try:
        transcript.extend(os.read(master, 4096))
    except OSError:
        break
after = echo_enabled()
normalized = bytes(transcript).replace(b"\r\n", b"\n")
result = {
    "echo_before": before,
    "echo_during": during,
    "echo_after": after,
    "secret_leaked": secret in transcript,
    "newline_after_prompt": prompt + b"\nVALUE_LENGTH=" in normalized,
    "value_accepted": (b"VALUE_LENGTH=" + str(len(secret)).encode("ascii") + b"\n") in normalized,
    "exit_code": exit_code,
}
print(json.dumps(result))
os.close(master)
os.close(slave)
`
	if err := os.WriteFile(controller, []byte(controllerSource), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := "pty-discord-token-never-print"
	cmd := exec.Command("python3", controller, harness)
	cmd.Stdin = strings.NewReader(secret + "\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run PTY prompt probe: %v: %s", err, output)
	}
	var probe secretPromptProbe
	if err := json.Unmarshal(output, &probe); err != nil {
		t.Fatalf("decode PTY prompt probe: %v", err)
	}
	return probe
}

type setupTransferResult struct {
	output            string
	sshLog            string
	scpLog            string
	localTempRoot     string
	localEnvPath      string
	localDirMode      string
	localEnvMode      string
	remoteStagingMode string
	remoteMarker      string
	secrets           []string
	err               error
}

func runRemoteSetupTransfer(t *testing.T, root, remotePath string, installExit int, failEnvCopy bool, sharedLocalTempRoot string) setupTransferResult {
	t.Helper()
	testRoot := t.TempDir()
	fakeBin := filepath.Join(testRoot, "bin")
	localTempRoot := sharedLocalTempRoot
	if localTempRoot == "" {
		localTempRoot = filepath.Join(testRoot, "tmp")
	}
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(localTempRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sshLog := filepath.Join(testRoot, "ssh.log")
	scpLog := filepath.Join(testRoot, "scp.log")
	localModeLog := filepath.Join(testRoot, "local-mode.log")
	remoteModeLog := filepath.Join(testRoot, "remote-mode.log")
	remoteMarker := filepath.Join(testRoot, "remote-temp-exists")
	writeExecutable(t, filepath.Join(fakeBin, "make"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "scp"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$SCP_LOG"
case "$*" in
  *"/valorant.env"*)
    source_path="$2"
    python3 - "$source_path" "$LOCAL_MODE_LOG" <<'PY'
import os
import stat
import sys

path = sys.argv[1]
with open(sys.argv[2], "a", encoding="utf-8") as mode_log:
    mode_log.write(
        path + "\t" +
        format(stat.S_IMODE(os.stat(os.path.dirname(path)).st_mode), "04o") + "\t" +
        format(stat.S_IMODE(os.stat(path).st_mode), "04o") + "\n"
    )
PY
    if [ "$FAIL_ENV_COPY" = "1" ]; then
      exit 23
    fi
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(fakeBin, "ssh"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$SSH_LOG"
last=""
for argument in "$@"; do
  last="$argument"
done
case "$last" in
  *"mktemp -d /tmp/valorant-bot-install.XXXXXX"*)
    : > "$REMOTE_MARKER"
    case "$last" in
      *"umask 077;"*) printf '0700' > "$REMOTE_MODE_LOG" ;;
      *) printf '0755' > "$REMOTE_MODE_LOG" ;;
    esac
    printf '%s\n' "$REMOTE_TEMP_PATH"
    ;;
  *"sudo ./deploy/install.sh"*)
    exit "$INSTALL_EXIT"
    ;;
  *"rm -f --"*"rmdir --"*)
    rm -f "$REMOTE_MARKER"
    ;;
  *"hostname -I"*)
    printf '192.0.2.50\n'
    ;;
esac
exit 0
`)
	secrets := []string{"discord-token-never-log", "bot-secret-never-log"}
	failCopy := "0"
	if failEnvCopy {
		failCopy = "1"
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "setup-pi.sh"),
		"--host", "pi@example.test", "--yes", "--skip-start")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+localTempRoot,
		"SSH_LOG="+sshLog,
		"SCP_LOG="+scpLog,
		"LOCAL_MODE_LOG="+localModeLog,
		"REMOTE_MODE_LOG="+remoteModeLog,
		"REMOTE_MARKER="+remoteMarker,
		"REMOTE_TEMP_PATH="+remotePath,
		"INSTALL_EXIT="+strconv.Itoa(installExit),
		"FAIL_ENV_COPY="+failCopy,
		"DISCORD_TOKEN="+secrets[0],
		"DISCORD_APP_ID=test-app",
		"BOT_SECRET="+secrets[1],
	)
	output, runErr := cmd.CombinedOutput()
	readOptional := func(path string) string {
		contents, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return ""
			}
			t.Fatal(err)
		}
		return string(contents)
	}
	scpContents := readOptional(scpLog)
	localEnvPath := envSourceFromScpLog(scpContents)
	localDirMode, localEnvMode := localModesForEnv(t, readOptional(localModeLog), localEnvPath)
	return setupTransferResult{
		output:            string(output),
		sshLog:            readOptional(sshLog),
		scpLog:            scpContents,
		localTempRoot:     localTempRoot,
		localEnvPath:      localEnvPath,
		localDirMode:      localDirMode,
		localEnvMode:      localEnvMode,
		remoteStagingMode: readOptional(remoteModeLog),
		remoteMarker:      remoteMarker,
		secrets:           secrets,
		err:               runErr,
	}
}

func assertSetupSecretsAbsent(t *testing.T, result setupTransferResult) {
	t.Helper()
	for _, contents := range []string{result.output, result.sshLog, result.scpLog} {
		for _, secret := range result.secrets {
			if strings.Contains(contents, secret) {
				// Do not include either the transcript or credential in this
				// failure; both are sensitive if this assertion trips.
				t.Fatal("setup-pi exposed a credential in output or command logs")
			}
		}
	}
}

func assertRandomizedSetupPathsAndCleanup(t *testing.T, result setupTransferResult, remotePath string, installAttempted bool) {
	t.Helper()
	if result.localEnvPath == "" {
		t.Fatal("env scp did not expose its local source path to the fake")
	}
	assertTaskScopedLocalEnvRemoved(t, result.localTempRoot, result.localEnvPath, result.localDirMode, result.localEnvMode)
	if result.remoteStagingMode != "0700" {
		t.Fatalf("remote staging directory mode=%q, want 0700", result.remoteStagingMode)
	}
	for _, want := range []string{
		"pi@example.test:" + remotePath + "/valorant-bot",
		"pi@example.test:" + remotePath + "/valorant.env",
		"rm -f -- '" + remotePath + "/valorant-bot' '" + remotePath + "/valorant.env'",
		"rmdir -- '" + remotePath + "'",
	} {
		if !strings.Contains(result.scpLog+result.sshLog, want) {
			t.Fatalf("setup transfer/cleanup log missing randomized path contract %q", want)
		}
	}
	if installAttempted {
		for _, want := range []string{
			"--binary " + remotePath + "/valorant-bot",
			"--env " + remotePath + "/valorant.env",
		} {
			if !strings.Contains(result.sshLog, want) {
				t.Fatalf("remote install log missing randomized path contract %q", want)
			}
		}
	}
	if strings.Contains(result.scpLog, "pi@example.test:/tmp/valorant.env") {
		t.Fatal("setup used the fixed remote /tmp/valorant.env path")
	}
	for _, line := range strings.Split(result.scpLog, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[len(fields)-1] == "pi@example.test:/tmp/valorant-bot" {
			t.Fatal("setup used the fixed remote /tmp/valorant-bot path")
		}
	}
	if _, err := os.Stat(result.remoteMarker); !os.IsNotExist(err) {
		t.Fatal("remote task temp marker remained after setup exit")
	}
}

func assertTaskScopedLocalEnvRemoved(t *testing.T, localTempRoot, envPath, directoryMode, envMode string) {
	t.Helper()
	if filepath.Base(envPath) != "env" || !strings.HasPrefix(filepath.Base(filepath.Dir(envPath)), "valorant-bot-setup.") {
		t.Fatalf("local env path is not task-scoped under TMPDIR: %q", envPath)
	}
	if filepath.Clean(filepath.Dir(filepath.Dir(envPath))) != filepath.Clean(localTempRoot) {
		t.Fatalf("local env path escaped the controlled temp root: %q", envPath)
	}
	if directoryMode != "0700" {
		t.Fatalf("local task temp directory mode=%q, want 0700", directoryMode)
	}
	if envMode != "0600" {
		t.Fatalf("local secret env mode=%q, want 0600", envMode)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatal("local secret env remained after setup exit")
	}
	if _, err := os.Stat(filepath.Dir(envPath)); !os.IsNotExist(err) {
		t.Fatal("local task temp directory remained after setup exit")
	}
}

func localModesForEnv(t *testing.T, observations, envPath string) (string, string) {
	t.Helper()
	for _, line := range strings.Split(observations, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 3 && fields[0] == envPath {
			return fields[1], fields[2]
		}
	}
	return "", ""
}

func envSourceFromScpLog(log string) string {
	for _, line := range strings.Split(log, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.HasSuffix(fields[len(fields)-1], "/valorant.env") {
			return fields[len(fields)-2]
		}
	}
	return ""
}

type localSetupResult struct {
	output        string
	installLog    string
	localTempRoot string
	localEnvPath  string
	localDirMode  string
	localEnvMode  string
	secrets       []string
	err           error
}

func runLocalPiSetupFailure(t *testing.T, setup string) localSetupResult {
	t.Helper()
	testRoot := t.TempDir()
	repoRoot := filepath.Join(testRoot, "repo")
	fakeBin := filepath.Join(testRoot, "bin")
	localTempRoot := filepath.Join(testRoot, "tmp")
	for _, directory := range []string{filepath.Join(repoRoot, "scripts"), filepath.Join(repoRoot, "deploy"), fakeBin, localTempRoot} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setupPath := filepath.Join(repoRoot, "scripts", "setup-pi.sh")
	writeExecutable(t, setupPath, setup)
	installLog := filepath.Join(testRoot, "install.log")
	localModeLog := filepath.Join(testRoot, "local-mode.log")
	writeExecutable(t, filepath.Join(repoRoot, "deploy", "install.sh"), `#!/bin/sh
set -eu
printf '%s\n' "$*" > "$LOCAL_INSTALL_LOG"
previous=""
env_path=""
for argument in "$@"; do
  if [ "$previous" = "--env" ]; then
    env_path="$argument"
    break
  fi
  previous="$argument"
done
python3 - "$env_path" "$LOCAL_MODE_LOG" <<'PY'
import os
import stat
import sys

path = sys.argv[1]
with open(sys.argv[2], "w", encoding="utf-8") as mode_log:
    mode_log.write(
        path + "\t" +
        format(stat.S_IMODE(os.stat(os.path.dirname(path)).st_mode), "04o") + "\t" +
        format(stat.S_IMODE(os.stat(path).st_mode), "04o") + "\n"
    )
PY
exit 37
`)
	writeExecutable(t, filepath.Join(fakeBin, "id"), "#!/bin/sh\nif [ \"${1:-}\" = -u ]; then printf '0\\n'; exit 0; fi\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "hostname"), "#!/bin/sh\nif [ \"${1:-}\" = -I ]; then printf '192.0.2.60\\n'; exit 0; fi\nexit 1\n")
	secrets := []string{"local-discord-token-never-log", "local-bot-secret-never-log"}
	cmd := exec.Command(setupPath, "--yes", "--skip-start")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+localTempRoot,
		"LOCAL_INSTALL_LOG="+installLog,
		"LOCAL_MODE_LOG="+localModeLog,
		"DISCORD_TOKEN="+secrets[0],
		"DISCORD_APP_ID=test-app",
		"BOT_SECRET="+secrets[1],
	)
	output, runErr := cmd.CombinedOutput()
	contents, err := os.ReadFile(installLog)
	if err != nil {
		t.Fatalf("read fake local install log: %v", err)
	}
	fields := strings.Fields(string(contents))
	envPath := ""
	for index, field := range fields {
		if field == "--env" && index+1 < len(fields) {
			envPath = fields[index+1]
		}
	}
	modeContents, err := os.ReadFile(localModeLog)
	if err != nil {
		t.Fatalf("read fake local mode log: %v", err)
	}
	directoryMode, envMode := localModesForEnv(t, string(modeContents), envPath)
	return localSetupResult{
		output:        string(output),
		installLog:    string(contents),
		localTempRoot: localTempRoot,
		localEnvPath:  envPath,
		localDirMode:  directoryMode,
		localEnvMode:  envMode,
		secrets:       secrets,
		err:           runErr,
	}
}

func runInstallServiceActivation(t *testing.T, installer string, initiallyActive, failDisplayRestart bool) installActivationResult {
	t.Helper()
	const activationStart = "systemctl daemon-reload\n"
	start := strings.Index(installer, activationStart)
	if start < 0 {
		t.Fatal("installer activation section is missing")
	}

	testRoot := t.TempDir()
	fakeBin := filepath.Join(testRoot, "bin")
	stateDir := filepath.Join(testRoot, "state")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(testRoot, "env")
	if err := os.WriteFile(envPath, []byte("DISCORD_TOKEN=test-token\nBOT_SECRET=test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if initiallyActive {
		for _, unit := range []string{"display", "bot"} {
			if err := os.WriteFile(filepath.Join(stateDir, unit), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	writeExecutable(t, filepath.Join(fakeBin, "systemctl"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$SYSTEMCTL_LOG"

state_for_unit() {
  case "$1" in
    valorant-captcha-display) printf '%s/display' "$SYSTEMCTL_STATE_DIR" ;;
    valorant-bot) printf '%s/bot' "$SYSTEMCTL_STATE_DIR" ;;
    *) exit 90 ;;
  esac
}

start_unit() {
  state="$(state_for_unit "$1")"
  if [ ! -f "$state" ]; then
    cp "$SYSTEMCTL_STATE_DIR/desired" "$state"
  fi
}

command="$1"
shift
case "$command" in
  daemon-reload)
    printf 'new' > "$SYSTEMCTL_STATE_DIR/desired"
    ;;
  enable)
    if [ "${1:-}" = "--now" ]; then
      shift
      start_unit "$1"
    fi
    ;;
  start)
    start_unit "$1"
    ;;
  restart)
    unit="$1"
    if [ "$unit" = "valorant-captcha-display" ] && [ "$FAIL_DISPLAY_RESTART" = "1" ]; then
      exit 19
    fi
    cp "$SYSTEMCTL_STATE_DIR/desired" "$(state_for_unit "$unit")"
    ;;
  try-restart)
    state="$(state_for_unit "$1")"
    if [ -f "$state" ]; then
      cp "$SYSTEMCTL_STATE_DIR/desired" "$state"
    fi
    ;;
  --no-pager)
    ;;
  *)
    exit 91
    ;;
esac
`)

	harness := filepath.Join(testRoot, "activation.sh")
	harnessSource := "#!/usr/bin/env bash\nset -euo pipefail\nREMOTE_CAPTCHA=1\nSKIP_START=0\nENV_DST=\"$TEST_ENV_DST\"\n" + installer[start:]
	writeExecutable(t, harness, harnessSource)
	callLog := filepath.Join(testRoot, "systemctl.log")
	failValue := "0"
	if failDisplayRestart {
		failValue = "1"
	}
	cmd := exec.Command(harness)
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TEST_ENV_DST="+envPath,
		"SYSTEMCTL_LOG="+callLog,
		"SYSTEMCTL_STATE_DIR="+stateDir,
		"FAIL_DISPLAY_RESTART="+failValue,
	)
	output, runErr := cmd.CombinedOutput()
	readFile := func(path string) string {
		contents, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return ""
			}
			t.Fatal(err)
		}
		return string(contents)
	}
	return installActivationResult{
		output:            string(output),
		calls:             readFile(callLog),
		displayGeneration: readFile(filepath.Join(stateDir, "display")),
		botGeneration:     readFile(filepath.Join(stateDir, "bot")),
		err:               runErr,
	}
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
