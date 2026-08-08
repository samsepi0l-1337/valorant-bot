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
	captchaReaperMaxAttempts  = 5
)

type captchaBrowserController interface {
	Close() error
}

type captchaBrowserCloseError struct {
	ProcessExited bool
	Err           error
}

type captchaBrowserCloseAttempt struct {
	flow       *passwordFlow
	controller captchaBrowserController
}

func (e *captchaBrowserCloseError) Error() string {
	return e.Err.Error()
}

func (e *captchaBrowserCloseError) Unwrap() error {
	return e.Err
}

type chromeBrowserController struct {
	cmd              *exec.Cmd
	processOwner     *captchaProcessOwnership
	ownerUsesSeams   bool
	profileRoot      string
	profileDir       string
	exited           chan struct{}
	waitErr          error
	closeDevTools    func(context.Context, string) error
	terminateProcess func(*os.Process, <-chan struct{}) error
	waitProcessExit  func(*os.Process, <-chan struct{}, time.Duration) bool
	removeProfile    func(string, string) error
	closeMu          sync.Mutex
	closed           bool
}

// captchaProcessOwnership makes group disappearance monotonic for one
// controller. A numeric Unix PGID can be reused, so after this owner observes
// its group gone it must never consult or signal that number again.
type captchaProcessOwnership struct {
	mu                   sync.Mutex
	gone                 bool
	stableGroup          bool
	singleUseTermination bool
	terminationAttempted bool
	waitRaw              func(time.Duration) bool
	terminateRaw         func(func(time.Duration) bool) error
	releaseRaw           func() error
	releaseOnce          sync.Once
}

func trackCaptchaProcessOwnership(waitRaw func(time.Duration) bool, terminateRaw func(func(time.Duration) bool) error) *captchaProcessOwnership {
	return &captchaProcessOwnership{waitRaw: waitRaw, terminateRaw: terminateRaw}
}

func (o *captchaProcessOwnership) waitForExit(timeout time.Duration) bool {
	if o == nil {
		return true
	}
	o.mu.Lock()
	if o.gone {
		o.mu.Unlock()
		o.release()
		return true
	}
	if o.waitRaw != nil && o.waitRaw(timeout) {
		o.gone = true
		o.mu.Unlock()
		o.release()
		return true
	}
	o.mu.Unlock()
	return false
}

func (o *captchaProcessOwnership) terminate() error {
	if o == nil || o.waitForExit(0) {
		return nil
	}
	if o.terminateRaw == nil {
		return errors.New("captcha Chrome process termination is unavailable")
	}
	if o.singleUseTermination {
		o.mu.Lock()
		if o.terminationAttempted {
			o.mu.Unlock()
			return errors.New("captcha Chrome process group remains after its terminal signal")
		}
		o.terminationAttempted = true
		o.mu.Unlock()
	}
	return o.terminateRaw(o.waitForExit)
}

func (o *captchaProcessOwnership) release() {
	if o == nil || o.releaseRaw == nil {
		return
	}
	o.releaseOnce.Do(func() { _ = o.releaseRaw() })
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
		return cleanupUnstartedChromeLaunch(profileRoot, profileDir, launchErr)
	}
	cmd := chromeCommand(bin, flags)
	return startChromeLogged(cmd, profileRoot, profileDir)
}

func launchMacChrome(chromeArgs []string, profileRoot, profileDir string) (captchaBrowserController, error) {
	bin := findChromeBinary()
	if bin == "" {
		launchErr := fmt.Errorf("Chrome/Chromium not found — install Google Chrome on the bot machine, or use Riot Mobile QR")
		return cleanupUnstartedChromeLaunch(profileRoot, profileDir, launchErr)
	}
	return startChromeLogged(chromeCommand(bin, chromeArgs), profileRoot, profileDir)
}

func startChromeLogged(cmd *exec.Cmd, profileRoot, profileDir string) (captchaBrowserController, error) {
	return startChromeLoggedWithRemove(cmd, profileRoot, profileDir, removeCaptchaChromeProfile)
}

func startChromeLoggedWithRemove(cmd *exec.Cmd, profileRoot, profileDir string, removeProfile func(string, string) error) (captchaBrowserController, error) {
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	preparedOwner, err := prepareCaptchaProcess(cmd)
	if err != nil {
		return cleanupUnstartedChromeLaunch(profileRoot, profileDir, err)
	}
	controller := &chromeBrowserController{
		cmd:           cmd,
		processOwner:  preparedOwner,
		profileRoot:   profileRoot,
		profileDir:    profileDir,
		exited:        make(chan struct{}),
		closeDevTools: closeChromeViaDevTools,
		removeProfile: removeProfile,
	}
	if err := cmd.Start(); err != nil {
		log.Printf("captcha chrome start failed: %v", err)
		close(controller.exited)
		return cleanupFailedChromeLaunch(controller, fmt.Errorf("start chrome: %w", err))
	}
	controller.processOwner = completeCaptchaProcessOwnership(preparedOwner, cmd.Process, controller.exited)

	go func() {
		controller.waitErr = cmd.Wait()
		close(controller.exited)
	}()

	timer := time.NewTimer(chromeStartupProbeTimeout)
	defer timer.Stop()
	select {
	case <-controller.exited:
		var launchErr error
		if controller.waitErr != nil {
			log.Printf("captcha chrome start failed: %v", controller.waitErr)
			launchErr = fmt.Errorf("start chrome: %w", controller.waitErr)
		} else {
			launchErr = fmt.Errorf("start chrome: process exited before startup completed")
		}
		return cleanupFailedChromeLaunch(controller, launchErr)
	case <-timer.C:
		return controller, nil
	}
}

func cleanupFailedChromeLaunch(controller *chromeBrowserController, launchErr error) (captchaBrowserController, error) {
	closeErr := controller.Close()
	if closeErr == nil {
		return nil, launchErr
	}
	return controller, errors.Join(launchErr, closeErr)
}

func cleanupUnstartedChromeLaunch(profileRoot, profileDir string, launchErr error) (captchaBrowserController, error) {
	controller := &chromeBrowserController{
		profileRoot:   profileRoot,
		profileDir:    profileDir,
		exited:        make(chan struct{}),
		closeDevTools: closeChromeViaDevTools,
		removeProfile: removeCaptchaChromeProfile,
	}
	close(controller.exited)
	return cleanupFailedChromeLaunch(controller, launchErr)
}

func (c *chromeBrowserController) Close() error {
	if c == nil {
		return nil
	}
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	if err := c.close(); err != nil {
		return err
	}
	c.closed = true
	return nil
}

func (c *chromeBrowserController) close() error {
	profileErr := validateCaptchaChromeProfile(c.profileRoot, c.profileDir)
	closeDevTools := c.closeDevTools
	if closeDevTools == nil {
		closeDevTools = closeChromeViaDevTools
	}

	ownedProcess := c.processOwner != nil || (c.cmd != nil && c.cmd.Process != nil)
	processOwner := c.ownedProcessOwnership()
	processExited := !ownedProcess || processOwner.waitForExit(0)
	graceful := false
	if profileErr == nil && ownedProcess && !processExited {
		ctx, cancel := context.WithTimeout(context.Background(), devToolsCloseTimeout)
		graceful = closeDevTools(ctx, c.profileDir) == nil
		cancel()
	}

	var terminateErr error
	if ownedProcess && !processExited {
		if processOwner.stableGroup || !graceful || !processOwner.waitForExit(chromeExitTimeout) {
			terminateErr = processOwner.terminate()
		}
	}

	processExited = !ownedProcess || processOwner.waitForExit(0)
	if ownedProcess && !processExited && terminateErr == nil {
		terminateErr = fmt.Errorf("captcha Chrome process did not exit")
	}
	var removeErr error
	if profileErr == nil && processExited {
		removeErr = c.removeOwnedProfile()
	}
	closeErr := errors.Join(profileErr, terminateErr, removeErr)
	if closeErr == nil {
		return nil
	}
	return &captchaBrowserCloseError{
		ProcessExited: processExited,
		Err:           closeErr,
	}
}

func (c *chromeBrowserController) ownedProcessOwnership() *captchaProcessOwnership {
	usesSeams := c.waitProcessExit != nil || c.terminateProcess != nil
	if c.processOwner != nil && (!usesSeams || c.ownerUsesSeams) {
		return c.processOwner
	}
	if c.cmd == nil || c.cmd.Process == nil {
		return c.processOwner
	}
	if !usesSeams {
		c.processOwner = newCaptchaProcessOwnership(c.cmd.Process, c.exited)
		return c.processOwner
	}
	if c.processOwner != nil {
		// Tests may replace one lifecycle operation after the production owner
		// has been prepared. Overlay only the requested operation: rebuilding
		// ownership around cmd.Process would discard a Unix guardian's stable
		// process-group identity and make the Chrome PID look already gone.
		if c.waitProcessExit != nil {
			waitProcessExit := c.waitProcessExit
			process := c.cmd.Process
			exited := c.exited
			c.processOwner.waitRaw = func(timeout time.Duration) bool {
				return waitProcessExit(process, exited, timeout)
			}
		}
		if c.terminateProcess != nil {
			terminateProcess := c.terminateProcess
			process := c.cmd.Process
			exited := c.exited
			c.processOwner.terminateRaw = func(func(time.Duration) bool) error {
				return terminateProcess(process, exited)
			}
		}
		c.ownerUsesSeams = true
		return c.processOwner
	}
	waitProcessExit := c.waitProcessExit
	if waitProcessExit == nil {
		waitProcessExit = waitForCaptchaOwnedProcessExit
	}
	terminateProcess := c.terminateProcess
	if terminateProcess == nil {
		terminateProcess = terminateCaptchaProcess
	}
	process := c.cmd.Process
	exited := c.exited
	c.processOwner = trackCaptchaProcessOwnership(
		func(timeout time.Duration) bool {
			return waitProcessExit(process, exited, timeout)
		},
		func(func(time.Duration) bool) error {
			return terminateProcess(process, exited)
		},
	)
	c.ownerUsesSeams = true
	return c.processOwner
}

func (c *chromeBrowserController) removeOwnedProfile() error {
	if err := validateCaptchaChromeProfile(c.profileRoot, c.profileDir); err != nil {
		return err
	}
	removeProfile := c.removeProfile
	if removeProfile == nil {
		removeProfile = removeCaptchaChromeProfile
	}
	return removeProfile(c.profileRoot, c.profileDir)
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
	out := s.passwordOutcomes[state]
	port := s.captchaTLSPort
	if !ok || pending.flow == nil || pending.flow.sealed || out.done ||
		pending.flow.ctx.Err() != nil || time.Now().After(pending.expiresAt) {
		s.mu.Unlock()
		return fmt.Errorf("captcha session expired; run /auth again")
	}
	flow := pending.flow
	s.mu.Unlock()

	flow.launchMu.Lock()
	if flow.ctx.Err() != nil {
		flow.launchMu.Unlock()
		return fmt.Errorf("captcha session expired; run /auth again")
	}
	s.mu.Lock()
	current, stillPending := s.passwordPending[state]
	out = s.passwordOutcomes[state]
	if !stillPending || current.flow != flow || flow.sealed || out.done || time.Now().After(current.expiresAt) {
		s.mu.Unlock()
		flow.launchMu.Unlock()
		return fmt.Errorf("captcha session expired; run /auth again")
	}
	if port <= 0 && !skipCaptchaTLSWait {
		s.mu.Unlock()
		flow.launchMu.Unlock()
		return fmt.Errorf("captcha TLS not started")
	}
	launcher := s.launchCaptchaBrowser
	s.mu.Unlock()

	old, closeErr := closeCaptchaBrowserLocked(flow)
	s.recordCaptchaBrowserCloseResultLocked(flow, old, closeErr, false)
	if closeErr != nil {
		flow.launchMu.Unlock()
		return fmt.Errorf("close existing captcha Chrome before reopen: %w", closeErr)
	}
	if flow.ctx.Err() != nil {
		flow.launchMu.Unlock()
		return fmt.Errorf("captcha session expired; run /auth again")
	}
	if launcher == nil {
		launcher = launchSystemChrome
	}
	widgetURL := s.captchaWidgetURL(state)
	controller, err := launcher(widgetURL)
	if err != nil {
		if controller != nil {
			// A failed process may still own a profile whose cleanup failed. Keep
			// the controller reachable so the bounded reaper and shutdown retry it.
			flow.browser = controller
			s.recordCaptchaBrowserCloseResultLocked(flow, controller, err, false)
		}
		flow.launchMu.Unlock()
		return err
	}
	if controller == nil {
		flow.launchMu.Unlock()
		return fmt.Errorf("captcha Chrome launcher returned no controller")
	}
	s.mu.Lock()
	current, stillPending = s.passwordPending[state]
	out = s.passwordOutcomes[state]
	live := stillPending && current.flow == flow && !flow.sealed && !out.done &&
		flow.ctx.Err() == nil && time.Now().Before(current.expiresAt)
	s.mu.Unlock()
	if !live {
		flow.browser = controller
		closedController, closeErr := closeCaptchaBrowserLocked(flow)
		s.recordCaptchaBrowserCloseResultLocked(flow, closedController, closeErr, false)
		flow.launchMu.Unlock()
		expiredErr := fmt.Errorf("captcha session expired; run /auth again")
		if closeErr != nil {
			return errors.Join(expiredErr, fmt.Errorf("close late captcha Chrome: %w", closeErr))
		}
		return expiredErr
	}
	flow.browser = controller
	flow.launchMu.Unlock()
	log.Printf("captcha Chrome launched")
	return nil
}

func closeCaptchaBrowserLocked(flow *passwordFlow) (captchaBrowserController, error) {
	if flow == nil {
		return nil, nil
	}
	controller := flow.browser
	if controller == nil {
		return nil, nil
	}
	if err := controller.Close(); err != nil {
		return controller, err
	}
	flow.browser = nil
	return controller, nil
}

func (s *Server) closeOwnedCaptchaBrowser(flow *passwordFlow) error {
	if flow == nil {
		return nil
	}
	flow.launchMu.Lock()
	controller, err := closeCaptchaBrowserLocked(flow)
	s.recordCaptchaBrowserCloseResultLocked(flow, controller, err, false)
	flow.launchMu.Unlock()
	return err
}

// recordCaptchaBrowserCloseResultLocked updates the retained-close record for
// controller. The caller must hold flow.launchMu so replacing flow.browser and
// publishing its close result are one atomic ownership transition.
func (s *Server) recordCaptchaBrowserCloseResultLocked(flow *passwordFlow, controller captchaBrowserController, closeErr error, preserveReaperAttempts bool) {
	if flow == nil || controller == nil {
		return
	}
	s.mu.Lock()
	if s.captchaCloseFailures == nil {
		s.captchaCloseFailures = make(map[*passwordFlow]captchaBrowserCloseFailure)
	}
	existing, alreadyRecorded := s.captchaCloseFailures[flow]
	sameRecord := alreadyRecorded && existing.controller == controller
	reaperAttempts := 0
	if preserveReaperAttempts && sameRecord {
		reaperAttempts = existing.reaperAttempts
	}
	startReaper := false
	if closeErr == nil {
		// A delayed success must not erase a newer controller's failure.
		if sameRecord {
			delete(s.captchaCloseFailures, flow)
		}
	} else if flow.browser == controller {
		// A delayed failure must not reclaim ownership after replacement.
		s.captchaCloseFailures[flow] = captchaBrowserCloseFailure{
			controller:      controller,
			err:             closeErr,
			possiblyRunning: captchaBrowserMayBeRunning(closeErr),
			reaperAttempts:  reaperAttempts,
		}
		if !s.closed && !s.captchaReaperRunning {
			s.captchaReaperRunning = true
			s.lifecycleWG.Add(1)
			startReaper = true
		}
	}
	s.mu.Unlock()
	if startReaper {
		go s.reapRetainedCaptchaBrowsers()
	}
	if closeErr != nil && !sameRecord && flow.browser == controller {
		log.Printf("captcha Chrome ownership retained after close failure: %v", closeErr)
	}
}

func (s *Server) retainedCaptchaBrowserCloseAttempts() []captchaBrowserCloseAttempt {
	s.mu.Lock()
	attempts := make([]captchaBrowserCloseAttempt, 0, len(s.captchaCloseFailures))
	for flow, failure := range s.captchaCloseFailures {
		attempts = append(attempts, captchaBrowserCloseAttempt{
			flow:       flow,
			controller: failure.controller,
		})
	}
	s.mu.Unlock()
	return attempts
}

// claimCaptchaBrowserReaperCloseAttempts gives each retained controller a
// bounded retry budget. A controller inserted during another controller's
// final round keeps its own untouched budget for the successor handoff.
func (s *Server) claimCaptchaBrowserReaperCloseAttempts() []captchaBrowserCloseAttempt {
	s.mu.Lock()
	attempts := make([]captchaBrowserCloseAttempt, 0, len(s.captchaCloseFailures))
	for flow, failure := range s.captchaCloseFailures {
		if failure.reaperAttempts >= captchaReaperMaxAttempts {
			continue
		}
		failure.reaperAttempts++
		s.captchaCloseFailures[flow] = failure
		attempts = append(attempts, captchaBrowserCloseAttempt{
			flow:       flow,
			controller: failure.controller,
		})
	}
	s.mu.Unlock()
	return attempts
}

func (s *Server) hasRetryableCaptchaBrowserFailureLocked() bool {
	for _, failure := range s.captchaCloseFailures {
		if failure.reaperAttempts < captchaReaperMaxAttempts {
			return true
		}
	}
	return false
}

// closeRetainedCaptchaBrowser retries only the controller captured in attempt.
// A stale retry is a no-op after another path has closed it or installed a
// replacement. Server.mu is never held while the controller performs I/O.
func (s *Server) closeRetainedCaptchaBrowser(attempt captchaBrowserCloseAttempt) error {
	flow := attempt.flow
	controller := attempt.controller
	if flow == nil || controller == nil {
		return nil
	}
	flow.launchMu.Lock()
	s.mu.Lock()
	failure, retained := s.captchaCloseFailures[flow]
	current := flow.browser
	matches := retained && failure.controller == controller && current == controller
	s.mu.Unlock()
	if !matches {
		flow.launchMu.Unlock()
		return nil
	}
	closedController, err := closeCaptchaBrowserLocked(flow)
	s.recordCaptchaBrowserCloseResultLocked(flow, closedController, err, true)
	flow.launchMu.Unlock()
	return err
}

func (s *Server) reapRetainedCaptchaBrowsers() {
	defer s.lifecycleWG.Done()
	defer s.finishCaptchaReaper()
	for round := 0; round < captchaReaperMaxAttempts; round++ {
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-timer.C:
		case <-s.lifecycleCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
		attempts := s.claimCaptchaBrowserReaperCloseAttempts()
		if len(attempts) == 0 {
			s.mu.Lock()
			empty := len(s.captchaCloseFailures) == 0
			s.mu.Unlock()
			if empty {
				if hook := s.beforeCaptchaReaperIdleExit; hook != nil {
					hook()
				}
			} else if hook := s.beforeCaptchaReaperMaxExit; hook != nil {
				hook()
			}
			return
		}
		for _, closeAttempt := range attempts {
			_ = s.closeRetainedCaptchaBrowser(closeAttempt)
		}
		s.mu.Lock()
		remaining := len(s.captchaCloseFailures)
		retryable := s.hasRetryableCaptchaBrowserFailureLocked()
		s.mu.Unlock()
		if remaining == 0 {
			if hook := s.beforeCaptchaReaperIdleExit; hook != nil {
				hook()
			}
			return
		}
		if !retryable {
			if hook := s.beforeCaptchaReaperMaxExit; hook != nil {
				hook()
			}
			return
		}
	}
	if hook := s.beforeCaptchaReaperMaxExit; hook != nil {
		hook()
	}
}

// finishCaptchaReaper closes every exit handoff without a missed wakeup. Only
// controllers with retry budget remaining can claim a successor, so persistent
// failures stay bounded while a newly inserted controller still gets retries.
func (s *Server) finishCaptchaReaper() {
	startSuccessor := false
	s.mu.Lock()
	s.captchaReaperRunning = false
	if !s.closed && s.hasRetryableCaptchaBrowserFailureLocked() {
		s.captchaReaperRunning = true
		s.lifecycleWG.Add(1)
		startSuccessor = true
	}
	s.mu.Unlock()
	if startSuccessor {
		go s.reapRetainedCaptchaBrowsers()
	}
}

func captchaBrowserMayBeRunning(closeErr error) bool {
	if closeErr == nil {
		return false
	}
	var controllerErr *captchaBrowserCloseError
	if errors.As(closeErr, &controllerErr) {
		return !controllerErr.ProcessExited
	}
	return true
}
