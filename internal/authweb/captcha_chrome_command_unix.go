//go:build unix

package authweb

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const captchaChromeExecArgument = "--valorant-internal-captcha-chrome-exec"
const captchaChromeExecEnvironment = "VALORANT_INTERNAL_CAPTCHA_CHROME_EXEC"
const captchaChromeExecPGIDEnvironment = "VALORANT_INTERNAL_CAPTCHA_CHROME_EXEC_PGID"

type captchaDesktopIdentity struct {
	uid    int
	gid    int
	groups []int
}

type captchaChromeCommandRuntime struct {
	goos           string
	effectiveUID   func() int
	desktopUser    func() string
	executable     func() (string, error)
	lookupIdentity func(string) (captchaDesktopIdentity, error)
	desktopEnv     func(string) []string
}

var currentCaptchaChromeCommandRuntime = captchaChromeCommandRuntime{
	goos:           runtime.GOOS,
	effectiveUID:   os.Geteuid,
	desktopUser:    desktopUser,
	executable:     os.Executable,
	lookupIdentity: lookupCaptchaDesktopIdentity,
	desktopEnv:     desktopEnv,
}

type captchaChromeExecSystem struct {
	geteuid   func() int
	getegid   func() int
	setpgid   func(int, int) error
	getpgrp   func() int
	setgroups func([]int) error
	setgid    func(int) error
	setuid    func(int) error
	exec      func(string, []string, []string) error
}

var defaultCaptchaChromeExecSystem = captchaChromeExecSystem{
	geteuid:   os.Geteuid,
	getegid:   os.Getegid,
	setpgid:   syscall.Setpgid,
	getpgrp:   syscall.Getpgrp,
	setgroups: syscall.Setgroups,
	setgid:    syscall.Setgid,
	setuid:    syscall.Setuid,
	exec:      syscall.Exec,
}

func chromeCommand(bin string, args []string) (*exec.Cmd, error) {
	runtime := currentCaptchaChromeCommandRuntime
	effectiveUID := -1
	if runtime.effectiveUID != nil {
		effectiveUID = runtime.effectiveUID()
	}
	username := ""
	if runtime.desktopUser != nil {
		username = strings.TrimSpace(runtime.desktopUser())
	}
	if effectiveUID != 0 || username == "" || username == "root" {
		return exec.Command(bin, args...), nil
	}
	if runtime.lookupIdentity == nil || runtime.executable == nil {
		return nil, fmt.Errorf("desktop Chrome identity helper is unavailable")
	}
	identity, err := runtime.lookupIdentity(username)
	if err != nil {
		return nil, fmt.Errorf("resolve desktop Chrome identity %q: %w", username, err)
	}
	if identity.uid < 0 || identity.gid < 0 {
		return nil, fmt.Errorf("invalid desktop Chrome identity %q", username)
	}
	executable, err := runtime.executable()
	if err != nil {
		return nil, fmt.Errorf("resolve desktop Chrome exec helper: %w", err)
	}
	if strings.TrimSpace(executable) == "" {
		return nil, fmt.Errorf("desktop Chrome exec helper path is empty")
	}
	groups := normalizedCaptchaDesktopGroups(identity.gid, identity.groups)
	helperArgs := []string{
		captchaChromeExecArgument,
		strconv.Itoa(identity.uid),
		strconv.Itoa(identity.gid),
		joinCaptchaGroupIDs(groups),
		bin,
	}
	helperArgs = append(helperArgs, args...)
	environment := os.Environ()
	if runtime.desktopEnv != nil {
		environment = runtime.desktopEnv(username)
	}
	environment = setCaptchaEnvironment(environment, captchaChromeExecEnvironment, "1")

	var cmd *exec.Cmd
	if runtime.goos == "darwin" {
		launchctlArgs := []string{"asuser", strconv.Itoa(identity.uid), executable}
		launchctlArgs = append(launchctlArgs, helperArgs...)
		cmd = exec.Command("launchctl", launchctlArgs...)
	} else {
		cmd = exec.Command(executable, helperArgs...)
	}
	cmd.Env = environment
	return cmd, nil
}

func lookupCaptchaDesktopIdentity(username string) (captchaDesktopIdentity, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return captchaDesktopIdentity{}, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return captchaDesktopIdentity{}, fmt.Errorf("parse uid: %w", err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return captchaDesktopIdentity{}, fmt.Errorf("parse gid: %w", err)
	}
	groupStrings, err := u.GroupIds()
	if err != nil {
		return captchaDesktopIdentity{}, fmt.Errorf("resolve supplementary groups: %w", err)
	}
	groups := make([]int, 0, len(groupStrings))
	for _, groupString := range groupStrings {
		groupID, parseErr := strconv.Atoi(groupString)
		if parseErr != nil {
			return captchaDesktopIdentity{}, fmt.Errorf("parse supplementary group: %w", parseErr)
		}
		groups = append(groups, groupID)
	}
	return captchaDesktopIdentity{uid: uid, gid: gid, groups: groups}, nil
}

func normalizedCaptchaDesktopGroups(primary int, groups []int) []int {
	unique := map[int]struct{}{primary: {}}
	for _, group := range groups {
		if group >= 0 {
			unique[group] = struct{}{}
		}
	}
	normalized := make([]int, 0, len(unique))
	for group := range unique {
		normalized = append(normalized, group)
	}
	sort.Ints(normalized)
	return normalized
}

func joinCaptchaGroupIDs(groups []int) string {
	values := make([]string, len(groups))
	for i, group := range groups {
		values[i] = strconv.Itoa(group)
	}
	return strings.Join(values, ",")
}

func parseCaptchaGroupIDs(value string) ([]int, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("desktop Chrome supplementary groups are empty")
	}
	parts := strings.Split(value, ",")
	groups := make([]int, 0, len(parts))
	for _, part := range parts {
		group, err := strconv.Atoi(part)
		if err != nil || group < 0 {
			return nil, fmt.Errorf("invalid desktop Chrome supplementary group %q", part)
		}
		groups = append(groups, group)
	}
	return groups, nil
}

func runCaptchaChromeExecHelper(args, environment []string, system captchaChromeExecSystem) error {
	if len(args) < 5 || args[0] != captchaChromeExecArgument {
		return fmt.Errorf("invalid desktop Chrome exec helper arguments")
	}
	if captchaEnvironmentValue(environment, captchaChromeExecEnvironment) != "1" {
		return fmt.Errorf("desktop Chrome exec helper marker is missing")
	}
	uid, err := strconv.Atoi(args[1])
	if err != nil || uid < 0 {
		return fmt.Errorf("invalid desktop Chrome uid %q", args[1])
	}
	gid, err := strconv.Atoi(args[2])
	if err != nil || gid < 0 {
		return fmt.Errorf("invalid desktop Chrome gid %q", args[2])
	}
	groups, err := parseCaptchaGroupIDs(args[3])
	if err != nil {
		return err
	}
	target := strings.TrimSpace(args[4])
	if target == "" {
		return fmt.Errorf("desktop Chrome executable is empty")
	}
	pgidValue := captchaEnvironmentValue(environment, captchaChromeExecPGIDEnvironment)
	pgid, err := strconv.Atoi(pgidValue)
	if err != nil || pgid <= 0 {
		return fmt.Errorf("invalid owned desktop Chrome process group %q", pgidValue)
	}
	if err := system.setpgid(0, pgid); err != nil {
		return fmt.Errorf("join owned desktop Chrome process group %d: %w", pgid, err)
	}
	if got := system.getpgrp(); got != pgid {
		return fmt.Errorf("desktop Chrome process group=%d, want owned group=%d", got, pgid)
	}

	if system.geteuid() == 0 {
		if err := system.setgroups(groups); err != nil {
			return fmt.Errorf("set desktop Chrome supplementary groups: %w", err)
		}
		if err := system.setgid(gid); err != nil {
			return fmt.Errorf("set desktop Chrome gid: %w", err)
		}
		if err := system.setuid(uid); err != nil {
			return fmt.Errorf("set desktop Chrome uid: %w", err)
		}
	} else if system.geteuid() != uid || system.getegid() != gid {
		return fmt.Errorf("desktop Chrome helper lacks privilege to become uid=%d gid=%d", uid, gid)
	}
	if system.geteuid() != uid || system.getegid() != gid {
		return fmt.Errorf("desktop Chrome identity verification failed")
	}
	if got := system.getpgrp(); got != pgid {
		return fmt.Errorf("final desktop Chrome process group=%d, want owned group=%d", got, pgid)
	}
	execArgs := append([]string{target}, args[5:]...)
	cleanEnvironment := removeCaptchaEnvironment(environment, captchaChromeExecEnvironment, captchaChromeExecPGIDEnvironment)
	if err := system.exec(target, execArgs, cleanEnvironment); err != nil {
		return fmt.Errorf("exec desktop Chrome: %w", err)
	}
	return nil
}

func isCaptchaChromeExecCommand(cmd *exec.Cmd) bool {
	if cmd == nil || captchaEnvironmentValue(cmd.Env, captchaChromeExecEnvironment) != "1" {
		return false
	}
	for _, arg := range cmd.Args {
		if arg == captchaChromeExecArgument {
			return true
		}
	}
	return false
}

func setCaptchaEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func removeCaptchaEnvironment(environment []string, keys ...string) []string {
	prefixes := make([]string, len(keys))
	for i, key := range keys {
		prefixes[i] = key + "="
	}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		remove := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry, prefix) {
				remove = true
				break
			}
		}
		if !remove {
			result = append(result, entry)
		}
	}
	return result
}

func captchaEnvironmentValue(environment []string, key string) string {
	prefix := key + "="
	for i := len(environment) - 1; i >= 0; i-- {
		if strings.HasPrefix(environment[i], prefix) {
			return strings.TrimPrefix(environment[i], prefix)
		}
	}
	return ""
}
