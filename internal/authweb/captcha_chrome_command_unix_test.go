//go:build unix

package authweb

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const captchaChromeExecTargetEnvironment = "VALORANT_CAPTCHA_CHROME_EXEC_TARGET"
const captchaChromeExecTargetPGIDFileEnvironment = "VALORANT_CAPTCHA_CHROME_EXEC_TARGET_PGID_FILE"

func installCaptchaChromeCommandRuntime(t *testing.T, runtime captchaChromeCommandRuntime) {
	t.Helper()
	original := currentCaptchaChromeCommandRuntime
	currentCaptchaChromeCommandRuntime = runtime
	t.Cleanup(func() { currentCaptchaChromeCommandRuntime = original })
}

func fakeCaptchaDesktopIdentity() captchaDesktopIdentity {
	return captchaDesktopIdentity{uid: 501, gid: 20, groups: []int{20, 80}}
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
	if os.Getenv(captchaChromeExecTargetEnvironment) != "1" {
		return
	}
	path := os.Getenv(captchaChromeExecTargetPGIDFileEnvironment)
	if err := os.WriteFile(path, []byte(strconv.Itoa(syscall.Getpgrp())), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

// Mutation caught: omitting guardian PGID injection or bypassing the helper
// means the actual final exec target cannot report the prepared guardian PGID.
func TestChromeCommandOwnedExecHelperRetainsGuardianPGID(t *testing.T) {
	identity := currentCaptchaDesktopIdentity(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	installCaptchaChromeCommandRuntime(t, captchaChromeCommandRuntime{
		goos:           "linux",
		effectiveUID:   func() int { return 0 },
		desktopUser:    func() string { return "desktop-test" },
		executable:     func() (string, error) { return executable, nil },
		lookupIdentity: func(string) (captchaDesktopIdentity, error) { return identity, nil },
		desktopEnv:     func(string) []string { return os.Environ() },
	})

	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("8", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pgidFile := filepath.Join(root, "final-pgid")
	t.Setenv(captchaChromeExecTargetEnvironment, "1")
	t.Setenv(captchaChromeExecTargetPGIDFileEnvironment, pgidFile)
	cmd, err := chromeCommand(executable, []string{"-test.run=^TestCaptchaChromeOwnedExecTarget$", "--"})
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
