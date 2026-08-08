//go:build unix

package authweb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var captchaDesktopEnvironmentSafeTestValues = []string{
	"HOME=/home/security-desktop-test",
	"USER=security-desktop-test",
	"LOGNAME=security-desktop-test",
	"PATH=/security/safe/bin:/usr/bin:/bin",
	"TMPDIR=/tmp/security-desktop-test",
	"DISPLAY=:77",
	"WAYLAND_DISPLAY=wayland-security-test",
	"XAUTHORITY=/tmp/security-xauthority-test",
	"DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/security-dbus-test",
	"XDG_RUNTIME_DIR=/run/user/501-security-test",
	"LANG=ko_KR.UTF-8",
	"LC_CTYPE=ko_KR.UTF-8",
	"__CF_USER_TEXT_ENCODING=0x1F5:0x3:0x33",
}

var captchaDesktopEnvironmentSecretTestValues = []string{
	"DISCORD_TOKEN=security-followup-discord-sentinel",
	"BOT_SECRET=security-followup-bot-sentinel",
	"RIOT_SECURITY_FOLLOWUP=security-followup-riot-sentinel",
	"AWS_SECRET_ACCESS_KEY=security-followup-cloud-sentinel",
	"DATABASE_URL=postgres://security-followup-database-sentinel",
	"HTTPS_PROXY=http://security-user:security-followup-proxy-sentinel@proxy.invalid",
	"UNKNOWN_SECURITY_FOLLOWUP=security-followup-unknown-sentinel",
}

type captchaChromeExecTargetObservation struct {
	Environment map[string]string `json:"environment"`
}

func installCaptchaChromeCommandRuntime(t *testing.T, runtime captchaChromeCommandRuntime) {
	t.Helper()
	original := currentCaptchaChromeCommandRuntime
	currentCaptchaChromeCommandRuntime = runtime
	t.Cleanup(func() { currentCaptchaChromeCommandRuntime = original })
}

func fakeCaptchaDesktopIdentity() captchaDesktopIdentity {
	return captchaDesktopIdentity{uid: 501, gid: 20, groups: []int{20, 80}}
}

// Mutation caught: copying the injected environment wholesale, or filtering
// only known credential prefixes, exposes arbitrary parent values to the
// privileged desktop helper.
func TestDesktopChromeHelperEnvironmentUsesExplicitAllowlist(t *testing.T) {
	injected := append([]string(nil), captchaDesktopEnvironmentSafeTestValues...)
	injected = append(injected, captchaDesktopEnvironmentSecretTestValues...)
	installCaptchaChromeCommandRuntime(t, captchaChromeCommandRuntime{
		goos:           "linux",
		effectiveUID:   func() int { return 0 },
		desktopUser:    func() string { return "security-desktop-test" },
		executable:     func() (string, error) { return "/opt/valorant-bot", nil },
		lookupIdentity: func(string) (captchaDesktopIdentity, error) { return fakeCaptchaDesktopIdentity(), nil },
		desktopEnv:     func(string) []string { return append([]string(nil), injected...) },
	})

	cmd, err := chromeCommand("/opt/google-chrome", []string{"--incognito"})
	if err != nil {
		t.Fatal(err)
	}
	assertEnvironmentEntries(t, cmd.Env, captchaDesktopEnvironmentSafeTestValues)
	assertEnvironmentKeysAbsent(t, cmd.Env, captchaDesktopEnvironmentSecretTestValues)
	if !environmentContains(cmd.Env, captchaChromeExecEnvironment+"=1") {
		t.Fatal("owned exec helper marker is absent from helper environment")
	}
	if captchaEnvironmentValue(cmd.Env, captchaChromeExecPGIDEnvironment) != "" {
		t.Fatal("guardian PGID marker was added before process preparation")
	}
}

// Mutation caught: restoring desktopEnv's os.Environ copy makes these unique
// parent sentinels visible before the command-construction boundary.
func TestDesktopEnvUsesExplicitAllowlist(t *testing.T) {
	for _, entry := range captchaDesktopEnvironmentSafeTestValues[3:] {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid safe test environment key %q", key)
		}
		t.Setenv(key, value)
	}
	for _, entry := range captchaDesktopEnvironmentSecretTestValues {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid secret test environment key %q", key)
		}
		t.Setenv(key, value)
	}

	environment := desktopEnv("security-desktop-test")
	assertEnvironmentEntries(t, environment, captchaDesktopEnvironmentSafeTestValues[3:])
	assertEnvironmentKeysAbsent(t, environment, captchaDesktopEnvironmentSecretTestValues)
	for _, want := range []string{"HOME=/Users/security-desktop-test", "USER=security-desktop-test", "LOGNAME=security-desktop-test"} {
		if !environmentContains(environment, want) {
			t.Fatalf("derived desktop identity field %q is absent", strings.SplitN(want, "=", 2)[0])
		}
	}
}

// Mutation caught: restoring the sudo-only Linux root path bypasses the
// credential-free final-exec helper and its guardian group verification.
func TestChromeCommandRootDesktopUsesOwnedExecHelperWithoutSudo(t *testing.T) {
	installCaptchaChromeCommandRuntime(t, captchaChromeCommandRuntime{
		goos:           "linux",
		effectiveUID:   func() int { return 0 },
		desktopUser:    func() string { return "desktop-test" },
		executable:     func() (string, error) { return "/opt/valorant-bot", nil },
		lookupIdentity: func(string) (captchaDesktopIdentity, error) { return fakeCaptchaDesktopIdentity(), nil },
		desktopEnv:     func(string) []string { return []string{"HOME=/home/desktop-test"} },
	})

	cmd, err := chromeCommand("/opt/google-chrome", []string{"--incognito", "https://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if cmd.Path != "/opt/valorant-bot" || len(cmd.Args) < 7 || cmd.Args[1] != captchaChromeExecArgument {
		t.Fatalf("root desktop command does not enter owned exec helper: path=%q args=%v", cmd.Path, cmd.Args)
	}
	if strings.Contains(joined, "sudo") {
		t.Fatalf("root desktop command still depends on sudo mediation: %v", cmd.Args)
	}
	if !environmentContains(cmd.Env, captchaChromeExecEnvironment+"=1") {
		t.Fatalf("owned exec helper marker missing from environment: %v", cmd.Env)
	}
}

// Mutation caught: restoring `launchctl asuser UID sudo -u ...` on Darwin
// skips the final owned-PGID helper boundary.
func TestChromeCommandDarwinPreservesBootstrapThroughOwnedExecHelper(t *testing.T) {
	installCaptchaChromeCommandRuntime(t, captchaChromeCommandRuntime{
		goos:           "darwin",
		effectiveUID:   func() int { return 0 },
		desktopUser:    func() string { return "desktop-test" },
		executable:     func() (string, error) { return "/Applications/Valorant Bot.app/Contents/MacOS/bot", nil },
		lookupIdentity: func(string) (captchaDesktopIdentity, error) { return fakeCaptchaDesktopIdentity(), nil },
		desktopEnv:     func(string) []string { return []string{"HOME=/Users/desktop-test"} },
	})

	cmd, err := chromeCommand("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", []string{"--incognito"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cmd.Path) != "launchctl" {
		t.Fatalf("Darwin desktop command path=%q, want launchctl", cmd.Path)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"asuser", "501", "/Applications/Valorant Bot.app/Contents/MacOS/bot", captchaChromeExecArgument} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Darwin owned helper command missing %q: %v", want, cmd.Args)
		}
	}
	if strings.Contains(joined, "sudo") {
		t.Fatalf("Darwin desktop command still depends on sudo mediation: %v", cmd.Args)
	}
}

func TestChromeCommandNonRootLaunchRemainsDirect(t *testing.T) {
	t.Setenv("PATH", "/security/non-root/bin:/usr/bin:/bin")
	t.Setenv("DISCORD_TOKEN", "security-followup-non-root-discord-sentinel")
	t.Setenv("UNKNOWN_SECURITY_FOLLOWUP", "security-followup-non-root-unknown-sentinel")
	installCaptchaChromeCommandRuntime(t, captchaChromeCommandRuntime{
		goos:         "linux",
		effectiveUID: func() int { return 1000 },
		desktopUser:  func() string { return "desktop-test" },
		executable:   func() (string, error) { return "", errors.New("must not resolve helper") },
		lookupIdentity: func(string) (captchaDesktopIdentity, error) {
			return captchaDesktopIdentity{}, errors.New("must not resolve desktop identity")
		},
	})

	cmd, err := chromeCommand("/opt/google-chrome", []string{"--incognito"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/opt/google-chrome" || len(cmd.Args) != 2 || cmd.Args[1] != "--incognito" {
		t.Fatalf("non-root Chrome launch was wrapped: path=%q args=%v", cmd.Path, cmd.Args)
	}
	if !environmentContains(cmd.Env, "PATH=/security/non-root/bin:/usr/bin:/bin") {
		t.Fatal("non-root Chrome environment did not preserve the safe PATH field")
	}
	assertEnvironmentKeysAbsent(t, cmd.Env, []string{"DISCORD_TOKEN=x", "UNKNOWN_SECURITY_FOLLOWUP=x"})
}

// Mutations caught: bypassing the common command boundary with a raw
// exec.Command leaves Cmd.Env nil, while using the Unix case-sensitive list on
// Windows loses ordinary mixed-case fields such as Path and UserProfile.
func TestChromeCommandWindowsModeUsesCommonEnvironmentBoundary(t *testing.T) {
	safeEnvironment := []string{
		`Path=C:\security\bin;C:\Windows\System32`,
		`UserProfile=C:\Users\security-test`,
		"username=security-test",
		`AppData=C:\Users\security-test\AppData\Roaming`,
		`LOCALAPPDATA=C:\Users\security-test\AppData\Local`,
		"HomeDrive=C:",
		`HOMEPATH=\Users\security-test`,
		`SystemRoot=C:\Windows`,
		`windir=C:\Windows`,
		`TEMP=C:\Users\security-test\AppData\Local\Temp`,
		"Lang=ko_KR.UTF-8",
	}
	for _, entry := range safeEnvironment {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	for _, entry := range []string{
		"Discord_Token=final-hardening-discord-sentinel",
		"Bot_Secret=final-hardening-bot-sentinel",
		"Aws_Secret_Access_Key=final-hardening-cloud-sentinel",
		"Unknown_Final_Hardening=final-hardening-unknown-sentinel",
	} {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	installCaptchaChromeCommandRuntime(t, captchaChromeCommandRuntime{
		goos:         "windows",
		effectiveUID: func() int { return 1000 },
		desktopUser:  func() string { return "security-test" },
	})

	cmd, err := chromeCommand(`C:\Program Files\Google\Chrome\Application\chrome.exe`, []string{"--incognito"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != `C:\Program Files\Google\Chrome\Application\chrome.exe` || len(cmd.Args) != 2 || cmd.Args[1] != "--incognito" {
		t.Fatalf("Windows-mode direct Chrome launch was wrapped: path=%q args=%v", cmd.Path, cmd.Args)
	}
	if cmd.Env == nil {
		t.Fatal("common Chrome command boundary left Windows-mode Cmd.Env nil")
	}
	assertEnvironmentEntries(t, cmd.Env, safeEnvironment)
	assertEnvironmentKeysAbsentFold(t, cmd.Env, []string{
		"DISCORD_TOKEN",
		"BOT_SECRET",
		"AWS_SECRET_ACCESS_KEY",
		"UNKNOWN_FINAL_HARDENING",
	})
}

// Mutation caught: case-sensitive Windows filtering drops ordinary Path and
// mixed-case identity fields; copying the input retains the dirty tail.
func TestWindowsDesktopEnvironmentAllowlistIsCaseInsensitive(t *testing.T) {
	wanted := []string{
		`Path=C:\security\new-bin`,
		`UserProfile=C:\Users\security-test`,
		"username=security-test",
		`AppData=C:\Users\security-test\AppData\Roaming`,
		`localappdata=C:\Users\security-test\AppData\Local`,
		"HomeDrive=C:",
		`homepath=\Users\security-test`,
		`SystemRoot=C:\Windows`,
		`windir=C:\Windows`,
		`Temp=C:\Users\security-test\AppData\Local\Temp`,
		"lang=ko_KR.UTF-8",
	}
	injected := []string{`PATH=C:\security\old-bin`}
	injected = append(injected, wanted...)
	injected = append(injected,
		"DISPLAY=:99",
		"Discord_Token=final-hardening-discord-sentinel",
		"Bot_Secret=final-hardening-bot-sentinel",
		"Aws_Secret_Access_Key=final-hardening-cloud-sentinel",
		"Unknown_Final_Hardening=final-hardening-unknown-sentinel",
	)

	filtered := allowlistedCaptchaDesktopEnvironment("windows", injected)
	if filtered == nil {
		t.Fatal("Windows allowlist returned nil environment")
	}
	assertEnvironmentEntries(t, filtered, wanted)
	if len(filtered) != len(wanted) {
		t.Fatalf("Windows allowlist retained %d entries, want exactly %d", len(filtered), len(wanted))
	}
	if environmentContains(filtered, `PATH=C:\security\old-bin`) {
		t.Fatal("Windows allowlist retained the superseded PATH spelling/value")
	}
}

// Mutation caught: deleting either final PGID verification lets the helper
// exec Chrome after a wrapper/session boundary placed it outside ownership.
func TestCaptchaChromeExecHelperFailsClosedOnFinalGroupMismatch(t *testing.T) {
	var execCalls atomic.Int32
	var requestedPGID atomic.Int32
	getpgrpCalls := atomic.Int32{}
	currentUID := atomic.Int32{}
	currentGID := atomic.Int32{}
	ops := captchaChromeExecSystem{
		geteuid: func() int { return int(currentUID.Load()) },
		getegid: func() int { return int(currentGID.Load()) },
		setpgid: func(pid, pgid int) error {
			if pid != 0 {
				t.Fatalf("setpgid pid=%d, want current process", pid)
			}
			requestedPGID.Store(int32(pgid))
			return nil
		},
		getpgrp: func() int {
			if getpgrpCalls.Add(1) == 1 {
				return 4242
			}
			return 4343
		},
		setgroups: func([]int) error { return nil },
		setgid: func(gid int) error {
			currentGID.Store(int32(gid))
			return nil
		},
		setuid: func(uid int) error {
			currentUID.Store(int32(uid))
			return nil
		},
		exec: func(string, []string, []string) error {
			execCalls.Add(1)
			return nil
		},
	}
	args := []string{captchaChromeExecArgument, "501", "20", "20,80", "/opt/google-chrome", "--incognito"}
	env := []string{captchaChromeExecEnvironment + "=1", captchaChromeExecPGIDEnvironment + "=4242"}

	err := runCaptchaChromeExecHelper(args, env, ops)
	if err == nil || !strings.Contains(err.Error(), "process group") {
		t.Fatalf("helper group mismatch error=%v", err)
	}
	if got := requestedPGID.Load(); got != 4242 {
		t.Fatalf("helper requested PGID=%d, want 4242", got)
	}
	if got := execCalls.Load(); got != 0 {
		t.Fatalf("helper exec calls=%d after final group mismatch, want 0", got)
	}
}

// This target runs only inside TestChromeCommandOwnedExecHelperRetainsGuardianPGID.
func TestCaptchaChromeOwnedExecTarget(t *testing.T) {
	targetArgs := argumentsAfterSeparator(os.Args)
	if len(targetArgs) != 2 {
		return
	}
	if err := os.WriteFile(targetArgs[0], []byte(strconv.Itoa(syscall.Getpgrp())), 0o600); err != nil {
		t.Fatal(err)
	}
	observedKeys := environmentKeys(captchaDesktopEnvironmentSafeTestValues, captchaDesktopEnvironmentSecretTestValues)
	observedKeys = append(observedKeys, captchaChromeExecEnvironment, captchaChromeExecPGIDEnvironment)
	observation := captchaChromeExecTargetObservation{Environment: make(map[string]string, len(observedKeys))}
	for _, key := range observedKeys {
		observation.Environment[key] = os.Getenv(key)
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetArgs[1], encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

// Mutation caught: replacing the helper's final explicit allowlist with
// marker-only deletion lets a valid direct helper invocation forward dirty
// environment values to its final exec target.
func TestCaptchaChromeExecHelperFiltersDirtyEnvironmentAtFinalExec(t *testing.T) {
	identity := currentCaptchaDesktopIdentity(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("7", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pgidFile := filepath.Join(root, "dirty-helper-final-pgid")
	environmentFile := filepath.Join(root, "dirty-helper-final-environment.json")
	safeEnvironment := setCaptchaEnvironment(captchaDesktopEnvironmentSafeTestValues, "TMPDIR", root)
	dirtyEnvironment := append([]string(nil), safeEnvironment...)
	dirtyEnvironment = append(dirtyEnvironment, captchaDesktopEnvironmentSecretTestValues...)
	dirtyEnvironment = setCaptchaEnvironment(dirtyEnvironment, captchaChromeExecEnvironment, "1")

	helperArgs := []string{
		captchaChromeExecArgument,
		strconv.Itoa(identity.uid),
		strconv.Itoa(identity.gid),
		joinCaptchaGroupIDs(normalizedCaptchaDesktopGroups(identity.gid, identity.groups)),
		executable,
		"-test.run=^TestCaptchaChromeOwnedExecTarget$",
		"--",
		pgidFile,
		environmentFile,
	}
	cmd := exec.Command(executable, helperArgs...)
	cmd.Env = dirtyEnvironment
	controllerValue, err := startChromeLogged(cmd, root, profileDir)
	if err != nil {
		t.Fatalf("start dirty helper final-exec target: %v", err)
	}
	controller := controllerValue.(*chromeBrowserController)
	controller.closeDevTools = func(context.Context, string) error {
		return errors.New("close dirty helper target through guardian group")
	}
	t.Cleanup(func() { _ = controller.Close() })

	observation := readCaptchaChromeTargetObservation(t, environmentFile)
	for _, entry := range safeEnvironment {
		key, want, _ := strings.Cut(entry, "=")
		if got := observation.Environment[key]; got != want {
			t.Fatalf("safe dirty-helper final environment key %q was not preserved", key)
		}
	}
	for _, key := range environmentKeys(captchaDesktopEnvironmentSecretTestValues) {
		if observation.Environment[key] != "" {
			t.Fatalf("forbidden dirty-helper final environment key %q was preserved", key)
		}
	}
	for _, key := range []string{captchaChromeExecEnvironment, captchaChromeExecPGIDEnvironment} {
		if observation.Environment[key] != "" {
			t.Fatalf("internal helper environment key %q reached dirty final target", key)
		}
	}
	if err := controller.Close(); err != nil {
		t.Fatalf("close dirty helper final-exec target: %v", err)
	}
}

// Mutation caught: omitting guardian PGID injection or bypassing the helper
// means the actual final exec target cannot report the prepared guardian PGID.
// Copying the parent environment, or retaining either internal helper marker,
// is caught by the same real helper-to-final-exec boundary.
func TestChromeCommandOwnedExecHelperRetainsGuardianPGID(t *testing.T) {
	identity := currentCaptchaDesktopIdentity(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	targetSafeEnvironment := setCaptchaEnvironment(captchaDesktopEnvironmentSafeTestValues, "TMPDIR", root)
	for _, entry := range append(append([]string(nil), targetSafeEnvironment...), captchaDesktopEnvironmentSecretTestValues...) {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid final target test environment key %q", key)
		}
		t.Setenv(key, value)
	}
	installCaptchaChromeCommandRuntime(t, captchaChromeCommandRuntime{
		goos:           "linux",
		effectiveUID:   func() int { return 0 },
		desktopUser:    func() string { return "security-desktop-test" },
		executable:     func() (string, error) { return executable, nil },
		lookupIdentity: func(string) (captchaDesktopIdentity, error) { return identity, nil },
		desktopEnv:     func(string) []string { return os.Environ() },
	})

	profileDir := filepath.Join(root, strings.Repeat("8", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pgidFile := filepath.Join(root, "final-pgid")
	environmentFile := filepath.Join(root, "final-environment.json")
	cmd, err := chromeCommand(executable, []string{"-test.run=^TestCaptchaChromeOwnedExecTarget$", "--", pgidFile, environmentFile})
	if err != nil {
		t.Fatal(err)
	}
	controllerValue, err := startChromeLogged(cmd, root, profileDir)
	if err != nil {
		t.Fatalf("start owned final-exec target: %v", err)
	}
	controller := controllerValue.(*chromeBrowserController)
	controller.closeDevTools = func(context.Context, string) error { return errors.New("close test target through guardian group") }
	t.Cleanup(func() { _ = controller.Close() })

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Pgid <= 0 {
		t.Fatal("default launcher did not prepare a guardian PGID")
	}
	deadline := time.Now().Add(2 * time.Second)
	var contents []byte
	for time.Now().Before(deadline) {
		contents, err = os.ReadFile(pgidFile)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read final target PGID: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("final exec target did not publish its PGID: %v", err)
	}
	gotPGID, err := strconv.Atoi(string(contents))
	if err != nil {
		t.Fatalf("parse final target PGID %q: %v", contents, err)
	}
	if gotPGID != cmd.SysProcAttr.Pgid {
		t.Fatalf("final exec target PGID=%d, want guardian PGID=%d", gotPGID, cmd.SysProcAttr.Pgid)
	}
	encodedEnvironment, err := os.ReadFile(environmentFile)
	if err != nil {
		t.Fatalf("read final target environment observation: %v", err)
	}
	var observation captchaChromeExecTargetObservation
	if err := json.Unmarshal(encodedEnvironment, &observation); err != nil {
		t.Fatalf("decode final target environment observation: %v", err)
	}
	for _, entry := range targetSafeEnvironment {
		key, want, _ := strings.Cut(entry, "=")
		if got := observation.Environment[key]; got != want {
			t.Fatalf("safe final target environment key %q was not preserved", key)
		}
	}
	for _, key := range environmentKeys(captchaDesktopEnvironmentSecretTestValues) {
		if observation.Environment[key] != "" {
			t.Fatalf("forbidden final target environment key %q was preserved", key)
		}
	}
	for _, key := range []string{captchaChromeExecEnvironment, captchaChromeExecPGIDEnvironment} {
		if observation.Environment[key] != "" {
			t.Fatalf("internal helper environment key %q reached final target", key)
		}
	}
	if err := controller.Close(); err != nil {
		t.Fatalf("close owned final-exec target: %v", err)
	}
}

func currentCaptchaDesktopIdentity(t *testing.T) captchaDesktopIdentity {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(current.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(current.Gid)
	if err != nil {
		t.Fatal(err)
	}
	groupStrings, err := current.GroupIds()
	if err != nil {
		t.Fatal(err)
	}
	groups := make([]int, 0, len(groupStrings))
	for _, groupString := range groupStrings {
		groupID, parseErr := strconv.Atoi(groupString)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		groups = append(groups, groupID)
	}
	return captchaDesktopIdentity{uid: uid, gid: gid, groups: groups}
}

func environmentContains(environment []string, want string) bool {
	for _, entry := range environment {
		if entry == want {
			return true
		}
	}
	return false
}

func assertEnvironmentEntries(t *testing.T, environment, wanted []string) {
	t.Helper()
	for _, entry := range wanted {
		key, _, _ := strings.Cut(entry, "=")
		if !environmentContains(environment, entry) {
			t.Fatalf("required environment key %q was not preserved", key)
		}
	}
}

func assertEnvironmentKeysAbsent(t *testing.T, environment, forbidden []string) {
	t.Helper()
	for _, key := range environmentKeys(forbidden) {
		if captchaEnvironmentValue(environment, key) != "" {
			t.Fatalf("forbidden environment key %q was preserved", key)
		}
	}
}

func assertEnvironmentKeysAbsentFold(t *testing.T, environment, forbidden []string) {
	t.Helper()
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		for _, forbiddenKey := range forbidden {
			if strings.EqualFold(key, forbiddenKey) {
				t.Fatalf("forbidden case-insensitive environment key %q was preserved", forbiddenKey)
			}
		}
	}
}

func environmentKeys(groups ...[]string) []string {
	var keys []string
	for _, group := range groups {
		for _, entry := range group {
			key, _, _ := strings.Cut(entry, "=")
			keys = append(keys, key)
		}
	}
	return keys
}

func argumentsAfterSeparator(arguments []string) []string {
	for i, argument := range arguments {
		if argument == "--" {
			return arguments[i+1:]
		}
	}
	return nil
}

func readCaptchaChromeTargetObservation(t *testing.T, path string) captchaChromeExecTargetObservation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var encoded []byte
	var err error
	for time.Now().Before(deadline) {
		encoded, err = os.ReadFile(path)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read final target environment observation: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("final target did not publish its environment observation: %v", err)
	}
	var observation captchaChromeExecTargetObservation
	if err := json.Unmarshal(encoded, &observation); err != nil {
		t.Fatalf("decode final target environment observation: %v", err)
	}
	return observation
}
