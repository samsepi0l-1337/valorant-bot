package authweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	chromeStartupProbeTimeout = 250 * time.Millisecond
	devToolsCloseTimeout      = 750 * time.Millisecond
	chromeExitTimeout         = 2 * time.Second
)

type captchaBrowserController interface {
	Close() error
}

type chromeBrowserController struct {
	cmd           *exec.Cmd
	profileRoot   string
	profileDir    string
	exited        chan struct{}
	waitErr       error
	closeDevTools func(context.Context, string) error
	closeOnce     sync.Once
	closeErr      error
}

func launchSystemChrome(widgetURL string) (captchaBrowserController, error) {
	profileRoot, profileDir, flags, err := chromeLaunchConfig(widgetURL)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "darwin" {
		return launchMacChrome(flags, profileRoot, profileDir)
	}
	bin := findChromeBinary()
	if bin == "" {
		launchErr := fmt.Errorf("Chrome/Chromium not found — install Google Chrome on the bot machine, or use Riot Mobile QR")
		return nil, errors.Join(launchErr, removeCaptchaChromeProfile(profileRoot, profileDir))
	}
	cmd := chromeCommand(bin, flags)
	return startChromeLogged(cmd, profileRoot, profileDir)
}

func launchMacChrome(chromeArgs []string, profileRoot, profileDir string) (captchaBrowserController, error) {
	bin := findChromeBinary()
	if bin == "" {
		launchErr := fmt.Errorf("Chrome/Chromium not found — install Google Chrome on the bot machine, or use Riot Mobile QR")
		return nil, errors.Join(launchErr, removeCaptchaChromeProfile(profileRoot, profileDir))
	}
	return startChromeLogged(chromeCommand(bin, chromeArgs), profileRoot, profileDir)
}

func startChromeLogged(cmd *exec.Cmd, profileRoot, profileDir string) (captchaBrowserController, error) {
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	configureCaptchaProcess(cmd)
	if err := cmd.Start(); err != nil {
		log.Printf("captcha chrome start failed: %v", err)
		return nil, errors.Join(fmt.Errorf("start chrome: %w", err), removeCaptchaChromeProfile(profileRoot, profileDir))
	}

	controller := &chromeBrowserController{
		cmd:           cmd,
		profileRoot:   profileRoot,
		profileDir:    profileDir,
		exited:        make(chan struct{}),
		closeDevTools: closeChromeViaDevTools,
	}
	go func() {
		controller.waitErr = cmd.Wait()
		close(controller.exited)
	}()

	timer := time.NewTimer(chromeStartupProbeTimeout)
	defer timer.Stop()
	select {
	case <-controller.exited:
		removeErr := removeCaptchaChromeProfile(profileRoot, profileDir)
		if controller.waitErr != nil {
			log.Printf("captcha chrome start failed: %v", controller.waitErr)
			return nil, errors.Join(fmt.Errorf("start chrome: %w", controller.waitErr), removeErr)
		}
		return nil, errors.Join(fmt.Errorf("start chrome: process exited before startup completed"), removeErr)
	case <-timer.C:
		return controller, nil
	}
}

func (c *chromeBrowserController) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closeErr = c.close()
	})
	return c.closeErr
}

func (c *chromeBrowserController) close() error {
	profileErr := validateCaptchaChromeProfile(c.profileRoot, c.profileDir)
	closeDevTools := c.closeDevTools
	if closeDevTools == nil {
		closeDevTools = closeChromeViaDevTools
	}

	graceful := false
	if profileErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), devToolsCloseTimeout)
		graceful = closeDevTools(ctx, c.profileDir) == nil
		cancel()
	}

	var terminateErr error
	if c.cmd != nil && c.cmd.Process != nil && !waitForCaptchaProcessExit(c.exited, 0) {
		if !graceful || !waitForCaptchaProcessExit(c.exited, chromeExitTimeout) {
			terminateErr = terminateCaptchaProcess(c.cmd.Process, c.exited)
		}
	}

	var removeErr error
	if profileErr == nil {
		removeErr = os.RemoveAll(c.profileDir)
	}
	return errors.Join(profileErr, terminateErr, removeErr)
}

func waitForCaptchaProcessExit(exited <-chan struct{}, timeout time.Duration) bool {
	if exited == nil {
		return false
	}
	if timeout <= 0 {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-exited:
		return true
	case <-timer.C:
		return false
	}
}

func closeChromeViaDevTools(ctx context.Context, profileDir string) error {
	endpoint, err := discoverChromeDevToolsEndpoint(ctx, profileDir)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{
		NetDialContext:   (&net.Dialer{Timeout: devToolsCloseTimeout}).DialContext,
		HandshakeTimeout: devToolsCloseTimeout,
	}
	conn, response, err := dialer.DialContext(ctx, endpoint, nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("connect captcha Chrome DevTools: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(devToolsCloseTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set captcha Chrome DevTools deadline: %w", err)
	}
	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Browser.close"}); err != nil {
		return fmt.Errorf("close captcha Chrome via DevTools: %w", err)
	}
	return nil
}

func discoverChromeDevToolsEndpoint(ctx context.Context, profileDir string) (string, error) {
	activePortFile := filepath.Join(profileDir, "DevToolsActivePort")
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		contents, err := os.ReadFile(activePortFile)
		if err == nil {
			endpoint, parseErr := parseChromeDevToolsActivePort(contents)
			if parseErr == nil {
				return endpoint, nil
			}
			lastErr = parseErr
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("discover captcha Chrome DevTools: %w: %v", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func parseChromeDevToolsActivePort(contents []byte) (string, error) {
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("invalid DevToolsActivePort")
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid DevToolsActivePort port")
	}
	path := strings.TrimSpace(lines[1])
	if !strings.HasPrefix(path, "/devtools/browser/") || strings.Contains(path, "://") {
		return "", fmt.Errorf("invalid DevToolsActivePort path")
	}
	endpoint := url.URL{
		Scheme: "ws",
		Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Path:   path,
	}
	return endpoint.String(), nil
}

func validateCaptchaChromeProfile(profileRoot, profileDir string) error {
	root, err := filepath.Abs(filepath.Clean(profileRoot))
	if err != nil {
		return fmt.Errorf("resolve captcha Chrome profile root: %w", err)
	}
	dir, err := filepath.Abs(filepath.Clean(profileDir))
	if err != nil {
		return fmt.Errorf("resolve captcha Chrome state profile: %w", err)
	}
	base := filepath.Base(dir)
	decoded, decodeErr := hex.DecodeString(base)
	if root == dir || filepath.Dir(dir) != root || decodeErr != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != base {
		return fmt.Errorf("refusing to remove non-state captcha Chrome profile %q", profileDir)
	}
	return nil
}

func removeCaptchaChromeProfile(profileRoot, profileDir string) error {
	if err := validateCaptchaChromeProfile(profileRoot, profileDir); err != nil {
		return err
	}
	return os.RemoveAll(profileDir)
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

func chromeLaunchConfig(widgetURL string) (profileRoot, profileDir string, flags []string, err error) {
	hostRules := fmt.Sprintf(
		"MAP %s 127.0.0.1",
		RiotCaptchaHost,
	)
	// Only map the current authenticator origin. Mapping auth.riotgames.com
	// would mint a token for Riot's legacy OAuth host and get it rejected.
	profileRoot, profileDir, err = chromeUserDataDir(widgetURL)
	if err != nil {
		return "", "", nil, err
	}
	flags = []string{
		"--user-data-dir=" + profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--new-window",
		"--incognito",
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=0",
		"--host-resolver-rules=" + hostRules,
		"--ignore-certificate-errors",
		"--allow-insecure-localhost",
		"--disable-features=HttpsFirstBalancedModeAutoEnable",
		widgetURL,
	}
	return profileRoot, profileDir, flags, nil
}

func chromeFlags(widgetURL string) ([]string, error) {
	_, _, flags, err := chromeLaunchConfig(widgetURL)
	return flags, err
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

func chromeUserDataDir(widgetURL string) (root, dir string, err error) {
	parsed, err := url.Parse(widgetURL)
	if err != nil {
		return "", "", fmt.Errorf("captcha widget URL: %w", err)
	}
	state := strings.TrimSpace(parsed.Query().Get("state"))
	if state == "" {
		return "", "", fmt.Errorf("captcha widget URL missing state")
	}

	if chromeUserDataDirFn != nil {
		root, err = chromeUserDataDirFn()
	} else {
		root, err = chromeUserDataDirDefault()
	}
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(state))
	dir = filepath.Join(root, fmt.Sprintf("%x", sum[:16]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	desktop := desktopUser()
	if desktop != "" && desktop != "root" && os.Geteuid() == 0 {
		if err := chownPath(dir, desktop); err != nil {
			log.Printf("captcha Chrome session-dir chown: %v", err)
		}
	}
	return root, dir, nil
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

// ensureCaptchaLaunched replaces the Chrome controller owned by a live state.
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
	launcher := s.launchCaptchaBrowser
	s.mu.Unlock()

	old := flow.browser
	flow.browser = nil
	if old != nil {
		_ = old.Close()
	}
	if flow.ctx.Err() != nil {
		return fmt.Errorf("captcha session expired; run /auth again")
	}
	if launcher == nil {
		launcher = launchSystemChrome
	}
	widgetURL := s.captchaWidgetURL(state)
	controller, err := launcher(widgetURL)
	if err != nil {
		return err
	}
	if controller == nil {
		return fmt.Errorf("captcha Chrome launcher returned no controller")
	}
	s.mu.Lock()
	current, stillPending = s.passwordPending[state]
	live := stillPending && current.flow == flow && flow.ctx.Err() == nil && time.Now().Before(current.expiresAt)
	s.mu.Unlock()
	if !live {
		_ = controller.Close()
		return fmt.Errorf("captcha session expired; run /auth again")
	}
	flow.browser = controller
	log.Printf("captcha chrome launched state=%s url=%s", state, widgetURL)
	return nil
}

func detachAndCloseCaptchaBrowser(flow *passwordFlow) error {
	if flow == nil {
		return nil
	}
	flow.launchMu.Lock()
	controller := flow.browser
	flow.browser = nil
	flow.launchMu.Unlock()
	if controller == nil {
		return nil
	}
	return controller.Close()
}
