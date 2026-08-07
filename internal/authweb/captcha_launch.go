package authweb

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func launchSystemChrome(widgetURL string) error {
	flags, err := chromeFlags(widgetURL)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return launchMacChrome(flags)
	}
	bin := findChromeBinary()
	if bin == "" {
		return fmt.Errorf("Chrome/Chromium not found — install Google Chrome on the bot machine, or use Riot Mobile QR")
	}
	cmd := chromeCommand(bin, flags)
	return startChromeLogged(cmd)
}

func launchMacChrome(chromeArgs []string) error {
	bin := findChromeBinary()
	if bin == "" {
		return fmt.Errorf("Chrome/Chromium not found — install Google Chrome on the bot machine, or use Riot Mobile QR")
	}
	return startChromeLogged(chromeCommand(bin, chromeArgs))
}

func startChromeLogged(cmd *exec.Cmd) error {
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		log.Printf("captcha chrome start failed: %v", err)
		return fmt.Errorf("start chrome: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	report := func(err error) error {
		if err != nil {
			log.Printf("captcha chrome start failed: %v", err)
			return fmt.Errorf("start chrome: %w", err)
		}
		return nil
	}

	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		return report(err)
	case <-timer.C:
		go func() {
			_ = report(<-done)
		}()
		return nil
	}
}

// chromeCommand runs Chrome as the real desktop user when the bot was started with sudo.
func chromeCommand(bin string, args []string) *exec.Cmd {
	if u := desktopUser(); u != "" && u != "root" && os.Geteuid() == 0 {
		if runtime.GOOS == "darwin" {
			if uid, err := userUID(u); err == nil {
				full := append([]string{"asuser", uid, "sudo", "-u", u, bin}, args...)
				cmd := exec.Command("launchctl", full...)
				cmd.Env = desktopEnv(u)
				return cmd
			}
		}
		full := append([]string{"-u", u, bin}, args...)
		cmd := exec.Command("sudo", full...)
		cmd.Env = desktopEnv(u)
		return cmd
	}
	return exec.Command(bin, args...)
}

func chromeFlags(widgetURL string) ([]string, error) {
	hostRules := fmt.Sprintf(
		"MAP %s 127.0.0.1",
		RiotCaptchaHost,
	)
	// Only map the current authenticator origin. Mapping auth.riotgames.com
	// would mint a token for Riot's legacy OAuth host and get it rejected.
	dataDir, err := chromeUserDataDir(widgetURL)
	if err != nil {
		return nil, err
	}
	return []string{
		"--user-data-dir=" + dataDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--new-window",
		"--incognito",
		"--host-resolver-rules=" + hostRules,
		"--ignore-certificate-errors",
		"--allow-insecure-localhost",
		"--disable-features=HttpsFirstBalancedModeAutoEnable",
		widgetURL,
	}, nil
}

// chromeLaunchSpec remains for unit tests.
func chromeLaunchSpec(widgetURL string) (bin string, args []string, err error) {
	args, err = chromeFlags(widgetURL)
	if err != nil {
		return "", nil, err
	}
	if bin = findChromeBinary(); bin != "" {
		return bin, args, nil
	}
	return "", nil, fmt.Errorf("Chrome/Chromium not found — install Google Chrome on the bot machine, or use Riot Mobile QR")
}

func chromeUserDataDir(widgetURL string) (string, error) {
	parsed, err := url.Parse(widgetURL)
	if err != nil {
		return "", fmt.Errorf("captcha widget URL: %w", err)
	}
	state := strings.TrimSpace(parsed.Query().Get("state"))
	if state == "" {
		return "", fmt.Errorf("captcha widget URL missing state")
	}

	root := ""
	if chromeUserDataDirFn != nil {
		root, err = chromeUserDataDirFn()
	} else {
		root, err = chromeUserDataDirDefault()
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(state))
	dir := filepath.Join(root, fmt.Sprintf("%x", sum[:16]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	desktop := desktopUser()
	if desktop != "" && desktop != "root" && os.Geteuid() == 0 {
		if err := chownPath(dir, desktop); err != nil {
			log.Printf("captcha Chrome session-dir chown: %v", err)
		}
	}
	return dir, nil
}

// chromeUserDataDirFn is overridden in unit tests.
var chromeUserDataDirFn func() (string, error)

func chromeUserDataDirDefault() (string, error) {
	desktop := desktopUser()
	home := ""
	if desktop != "" && desktop != "root" {
		if u, err := user.Lookup(desktop); err == nil {
			home = u.HomeDir
		} else if runtime.GOOS == "darwin" {
			home = filepath.Join("/Users", desktop)
		}
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" || home == "/var/root" || home == "/root" {
		home = os.TempDir()
	}
	dir := filepath.Join(home, ".cache", "valorant-bot-captcha-chrome-authenticate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// Last resort: unique temp dir (avoids root-owned shared /tmp profile).
		tmp, terr := os.MkdirTemp("", "valorant-bot-captcha-chrome-*")
		if terr != nil {
			return "", err
		}
		dir = tmp
	}
	if desktop != "" && desktop != "root" && os.Geteuid() == 0 {
		if err := chownPath(dir, desktop); err != nil {
			log.Printf("captcha chrome user-data-dir chown: %v", err)
		}
	}
	return dir, nil
}

// Each auth state gets its own incognito Chrome process and cookie jar. The
// state is hashed so the bearer-like Discord auth state is not exposed in a
// filesystem path.
func captchaChromeProfileDir(home, state string) string {
	sum := sha256.Sum256([]byte(state))
	return filepath.Join(home, ".cache", "valorant-bot-captcha-chrome-authenticate", fmt.Sprintf("%x", sum[:16]))
}

func desktopUser() string {
	if u := strings.TrimSpace(os.Getenv("SUDO_USER")); u != "" && u != "root" {
		return u
	}
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" && u != "root" {
		return u
	}
	return ""
}

func desktopEnv(username string) []string {
	home := filepath.Join("/Users", username)
	if u, err := user.Lookup(username); err == nil && u.HomeDir != "" {
		home = u.HomeDir
	}
	env := os.Environ()
	filtered := make([]string, 0, len(env)+4)
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") || strings.HasPrefix(e, "USER=") || strings.HasPrefix(e, "LOGNAME=") {
			continue
		}
		filtered = append(filtered, e)
	}
	filtered = append(filtered,
		"HOME="+home,
		"USER="+username,
		"LOGNAME="+username,
	)
	return filtered
}

func userUID(username string) (string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return "", err
	}
	return u.Uid, nil
}

func chownPath(path, username string) error {
	u, err := user.Lookup(username)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

func findChromeBinary() string {
	candidates := chromeCandidates()
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

func chromeCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"google-chrome",
			"chromium",
			"chromium-browser",
		}
	case "linux":
		return []string{
			"google-chrome",
			"google-chrome-stable",
			"chromium",
			"chromium-browser",
			"chrome",
			"/usr/bin/google-chrome",
			"/usr/bin/chromium-browser",
			"/usr/bin/chromium",
		}
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		prog := os.Getenv("PROGRAMFILES")
		prog86 := os.Getenv("PROGRAMFILES(X86)")
		return []string{
			filepath.Join(local, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(prog, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(prog86, `Google\Chrome\Application\chrome.exe`),
			"chrome.exe",
		}
	default:
		return []string{"google-chrome", "chromium", "chromium-browser"}
	}
}

// ensureCaptchaLaunched starts Chrome once per state (idempotent for retries).
func (s *Server) ensureCaptchaLaunched(state string) error {
	state = strings.TrimSpace(state)
	if state == "" {
		return fmt.Errorf("missing state")
	}
	s.mu.Lock()
	pending, ok := s.passwordPending[state]
	port := s.captchaTLSPort
	if !ok || pending.flow == nil || pending.flow.ctx.Err() != nil || time.Now().After(pending.expiresAt) {
		s.mu.Unlock()
		return fmt.Errorf("captcha session expired; run /auth again")
	}
	flow := pending.flow
	s.mu.Unlock()

	flow.launchMu.Lock()
	defer flow.launchMu.Unlock()
	if flow.ctx.Err() != nil {
		return fmt.Errorf("captcha session expired; run /auth again")
	}
	s.mu.Lock()
	current, stillPending := s.passwordPending[state]
	if !stillPending || current.flow != flow || time.Now().After(current.expiresAt) {
		s.mu.Unlock()
		return fmt.Errorf("captcha session expired; run /auth again")
	}
	if port <= 0 {
		s.mu.Unlock()
		return fmt.Errorf("captcha TLS not started")
	}
	if s.captchaLaunched == nil {
		s.captchaLaunched = make(map[string]time.Time)
	}
	if t, alreadyLaunched := s.captchaLaunched[state]; alreadyLaunched && time.Since(t) < 2*time.Minute {
		s.mu.Unlock()
		return nil
	}
	s.captchaLaunched[state] = time.Now()
	launcher := s.launchCaptchaBrowser
	s.mu.Unlock()
	if launcher == nil {
		launcher = launchSystemChrome
	}
	url := s.captchaWidgetURL(state)
	if err := launcher(url); err != nil {
		s.mu.Lock()
		delete(s.captchaLaunched, state)
		s.mu.Unlock()
		return err
	}
	log.Printf("captcha chrome launched state=%s url=%s", state, url)
	return nil
}

// resetCaptchaLaunch allows the Discord owner button to force a new Chrome window.
func (s *Server) resetCaptchaLaunch(state string) {
	s.mu.Lock()
	if s.captchaLaunched != nil {
		delete(s.captchaLaunched, state)
	}
	s.mu.Unlock()
}
