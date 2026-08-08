package authweb

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestGenerateCaptchaSelfSigned(t *testing.T) {
	certPEM, keyPEM, err := generateCaptchaSelfSigned(RiotCaptchaHost)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(certPEM), "BEGIN CERTIFICATE") || !strings.Contains(string(keyPEM), "BEGIN EC PRIVATE KEY") {
		t.Fatalf("unexpected pem")
	}
}

func TestEnsureCaptchaTLSFiles(t *testing.T) {
	dir := t.TempDir()
	cert1, key1, err := ensureCaptchaTLSFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cert1); err != nil {
		t.Fatal(err)
	}
	cert2, key2, err := ensureCaptchaTLSFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cert1 != cert2 || key1 != key2 {
		t.Fatalf("expected reuse %q vs %q", cert1, cert2)
	}
	if filepath.Base(filepath.Dir(cert1)) != "captcha-tls" {
		t.Fatalf("dir=%s", cert1)
	}
}

func TestEnsureCaptchaTLSFilesRegeneratesLegacyHostCertificate(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "captcha-tls")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyCert, legacyKey, err := generateCaptchaSelfSigned("auth.riotgames.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "cert.pem"), legacyCert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "key.pem"), legacyKey, 0o600); err != nil {
		t.Fatal(err)
	}

	certFile, _, err := ensureCaptchaTLSFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("missing certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname(RiotCaptchaHost); err != nil {
		t.Fatalf("certificate was not regenerated for %s: %v", RiotCaptchaHost, err)
	}
}

func TestTLSStartupFailureDoesNotLaunchAgainstUnrelatedLoopbackTLS(t *testing.T) {
	occupant := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer occupant.Close()
	port := occupant.Listener.Addr().(*net.TCPAddr).Port

	s := New(Deps{
		CaptchaTLSPort: port,
		PasswordAuth:   &fakePasswordAuth{},
		PendingTTL:     time.Minute,
		Store:          newMockStore(),
		Riot:           &mockRiot{},
		Boxer:          &mockBoxer{},
	})
	var launchedURL string
	s.launchCaptchaBrowser = func(widgetURL string) (captchaBrowserController, error) {
		launchedURL = widgetURL
		return newTestCaptchaBrowserController(), nil
	}
	if err := s.StartCaptchaTLS(port, t.TempDir()); err == nil {
		t.Fatal("StartCaptchaTLS succeeded on an occupied port")
	}

	oldSkip := skipCaptchaTLSWait
	skipCaptchaTLSWait = false
	t.Cleanup(func() { skipCaptchaTLSWait = oldSkip })
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "user", "pass")
	if err != nil {
		t.Fatal(err)
	}
	err = s.LaunchPasswordCaptcha(context.Background(), state, "owner-1")
	if err == nil {
		t.Fatal("launch accepted an unrelated loopback TLS service as owned readiness")
	}
	if launchedURL != "" {
		t.Fatalf("state-bearing widget URL leaked to launcher after TLS ownership failure: %q", launchedURL)
	}
}

func TestChromeLaunchSpec(t *testing.T) {
	bin, args, err := chromeLaunchSpec("https://authenticate.riotgames.com:8443/captcha/widget?state=x")
	if err != nil {
		if !strings.Contains(err.Error(), "Chrome") && !strings.Contains(err.Error(), "Chromium") {
			t.Fatalf("err=%v", err)
		}
		return
	}
	if bin == "" {
		t.Fatal("empty bin")
	}
	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, "host-resolver-rules=") || !strings.Contains(joined, RiotCaptchaHost) {
		t.Fatalf("args=%v", args)
	}
}

func TestCaptchaWidgetUsesAuthenticateOrigin(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	s.captchaTLSPort = 8443

	got := s.captchaWidgetURL("state-1")
	want := "https://authenticate.riotgames.com:8443/captcha/widget?state=state-1"
	if got != want {
		t.Fatalf("captcha widget URL = %q, want %q", got, want)
	}

	args, err := chromeFlags(got)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, "MAP authenticate.riotgames.com 127.0.0.1") {
		t.Fatalf("Chrome must map the current Riot authenticator host: %v", args)
	}
	if strings.Contains(joined, "MAP auth.riotgames.com 127.0.0.1") {
		t.Fatalf("Chrome must not mint the captcha on the legacy auth host: %v", args)
	}
	if !strings.Contains(joined, "--remote-debugging-address=127.0.0.1") ||
		!strings.Contains(joined, "--remote-debugging-port=0") {
		t.Fatalf("Chrome must expose an ephemeral loopback DevTools endpoint: %v", args)
	}
}

func TestOfficialRiotChromeUsesNetworkWithoutLocalHostMapping(t *testing.T) {
	rawURL := "https://auth.riotgames.com/authorize?client_id=riot-client&nonce=official-state"
	args, err := chromeFlags(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\n")
	if strings.Contains(joined, "host-resolver-rules") ||
		strings.Contains(joined, "ignore-certificate-errors") ||
		strings.Contains(joined, "allow-insecure-localhost") {
		t.Fatalf("official Riot browser must use normal DNS/TLS: %v", args)
	}
	if !strings.Contains(joined, "--remote-debugging-pipe") ||
		strings.Contains(joined, "--remote-debugging-address") ||
		strings.Contains(joined, "--remote-debugging-port") ||
		!strings.Contains(joined, rawURL) {
		t.Fatalf("official Riot browser flags = %v", args)
	}
	_, firstProfile, _, err := chromeLaunchConfig(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	_, secondProfile, _, err := chromeLaunchConfig(strings.Replace(rawURL, "official-state", "other-state", 1))
	if err != nil {
		t.Fatal(err)
	}
	if firstProfile == secondProfile {
		t.Fatalf("official Riot browser profiles share %q", firstProfile)
	}
}

func TestCaptchaChromeProfilesAreIsolatedPerState(t *testing.T) {
	first := captchaChromeProfileDir("/Users/tester", "state-a")
	second := captchaChromeProfileDir("/Users/tester", "state-b")
	if first == second {
		t.Fatalf("concurrent captcha states share profile %q", first)
	}
	root := filepath.Join("/Users/tester", ".cache", "valorant-bot-captcha-chrome-authenticate")
	for _, got := range []string{first, second} {
		if !strings.HasPrefix(got, root+string(filepath.Separator)) {
			t.Fatalf("captcha Chrome profile %q is outside %q", got, root)
		}
		if strings.Contains(got, "state-a") || strings.Contains(got, "state-b") {
			t.Fatalf("captcha state must not be exposed in profile path: %q", got)
		}
	}

	firstURL := "https://authenticate.riotgames.com/captcha/widget?state=state-a"
	secondURL := "https://authenticate.riotgames.com/captcha/widget?state=state-b"
	firstFlags, err := chromeFlags(firstURL)
	if err != nil {
		t.Fatal(err)
	}
	secondFlags, err := chromeFlags(secondURL)
	if err != nil {
		t.Fatal(err)
	}
	firstJoined := strings.Join(firstFlags, "\n")
	secondJoined := strings.Join(secondFlags, "\n")
	if !strings.Contains(firstJoined, "--incognito") || !strings.Contains(secondJoined, "--incognito") {
		t.Fatalf("captcha profiles must run incognito: first=%v second=%v", firstFlags, secondFlags)
	}
	if firstJoined == secondJoined {
		t.Fatalf("concurrent captcha launches use identical flags: %v", firstFlags)
	}
}

func TestStartChromeLoggedDoesNotWaitForBrowserExit(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("a", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestChromeLaunchHelperProcess", "--")
	cmd.Env = append(os.Environ(), "VALORANT_CHROME_LAUNCH_HELPER=1")
	started := time.Now()
	controller, err := startChromeLogged(cmd, root, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed >= time.Second {
		t.Fatalf("Chrome launcher waited %s for the browser process; Discord launch must return immediately", elapsed)
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("fallback close retained state profile: %v", err)
	}
}

func TestStartChromeLoggedWiresOfficialDevToolsThroughPrivatePipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go ExtraFiles does not support the Chrome DevTools pipe on Windows")
	}
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("f", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestChromeLaunchHelperProcess", "--", "--remote-debugging-pipe")
	cmd.Env = append(os.Environ(), "VALORANT_CHROME_LAUNCH_HELPER=pipe")
	owned, err := startChromeLogged(cmd, root, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	controller := owned.(*chromeBrowserController)
	if controller.devToolsPipe == nil {
		t.Fatal("official Chrome controller has no private DevTools pipe")
	}
	client := &chromeDevToolsClient{conn: controller.devToolsPipe}
	var version struct {
		Product string `json:"product"`
	}
	if err := client.call("Browser.getVersion", map[string]any{}, &version); err != nil {
		t.Fatal(err)
	}
	if version.Product != "private-pipe-test" {
		t.Fatalf("private pipe product = %q", version.Product)
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartChromeLoggedReportsImmediateProcessFailure(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("d", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestChromeLaunchHelperProcess", "--")
	cmd.Env = append(os.Environ(), "VALORANT_CHROME_LAUNCH_HELPER=fail")
	controller, err := startChromeLogged(cmd, root, profileDir)
	if err == nil || controller != nil {
		t.Fatalf("controller=%T error=%v, want immediate launch failure", controller, err)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("failed launch retained state profile: %v", err)
	}
}

func TestImmediateLaunchFailureRetainsProfileForReaperRetry(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("2", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(profileDir, "challenge-state")
	if err := os.WriteFile(marker, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}

	var launchCalls atomic.Int32
	var removeCalls atomic.Int32
	temporaryRemoveFailure := errors.New("profile is temporarily locked")
	s := newCaptchaServer(&fakePasswordAuth{})
	s.launchCaptchaBrowser = func(string) (captchaBrowserController, error) {
		launchCalls.Add(1)
		cmd := exec.Command(os.Args[0], "-test.run=TestChromeLaunchHelperProcess", "--")
		cmd.Env = append(os.Environ(), "VALORANT_CHROME_LAUNCH_HELPER=fail")
		return startChromeLoggedWithRemove(cmd, root, profileDir, func(profileRoot, profileDir string) error {
			if removeCalls.Add(1) == 1 {
				return temporaryRemoveFailure
			}
			return removeCaptchaChromeProfile(profileRoot, profileDir)
		})
	}
	_, state, err := s.BeginPasswordLogin(context.Background(), "owner-1", "riot-user", "secret-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LaunchPasswordCaptcha(context.Background(), state, "owner-1"); !errors.Is(err, temporaryRemoveFailure) {
		t.Fatalf("launch error=%v, want retained cleanup failure", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(profileDir); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reaper did not remove retained failed-launch profile; remove calls=%d", removeCalls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := launchCalls.Load(); got != 1 {
		t.Fatalf("failed launch attempts=%d, want 1", got)
	}
	s.mu.Lock()
	pending := s.passwordPending[state]
	_, retained := s.captchaCloseFailures[pending.flow]
	s.mu.Unlock()
	pending.flow.launchMu.Lock()
	owned := pending.flow.browser
	pending.flow.launchMu.Unlock()
	if retained || owned != nil {
		t.Fatalf("successful reaper left failed-launch ownership: retained=%v browser=%T", retained, owned)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCaptchaBrowserCloseRemovesOnlyOwnedStateProfileWithoutDevTools(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("b", 32))
	siblingDir := filepath.Join(root, strings.Repeat("c", 32))
	for _, dir := range []string{profileDir, siblingDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("keep scope exact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exited := make(chan struct{})
	close(exited)
	controller := &chromeBrowserController{
		profileRoot: root,
		profileDir:  profileDir,
		exited:      exited,
		closeDevTools: func(context.Context, string) error {
			return errors.New("DevToolsActivePort unavailable")
		},
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("owned state profile still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(siblingDir, "marker")); err != nil {
		t.Fatalf("sibling profile was removed: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("shared profile root was removed: %v", err)
	}
}

func TestCaptchaBrowserCloseKeepsProfileUntilOwnedProcessExits(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("e", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "marker"), []byte("must remain while Chrome is live"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestChromeLaunchHelperProcess", "--")
	cmd.Env = append(os.Environ(), "VALORANT_CHROME_LAUNCH_HELPER=1")
	controller, err := startChromeLogged(cmd, root, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	chrome := controller.(*chromeBrowserController)
	chrome.closeDevTools = func(context.Context, string) error { return errors.New("DevTools unavailable") }

	// Simulate a failed process-group termination. The profile must remain owned
	// and intact until the process has actually exited, so a later close can
	// reclaim it safely.
	chrome.terminateProcess = func(*os.Process, <-chan struct{}) error {
		return errors.New("process group still running")
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-chrome.exited
		_ = os.RemoveAll(root)
	})

	closeErr := chrome.Close()
	if closeErr == nil || !captchaBrowserMayBeRunning(closeErr) {
		t.Fatalf("close error=%v, want an unexited-process error", closeErr)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "marker")); err != nil {
		t.Fatalf("live Chrome profile was removed before exit: %v", err)
	}
}

func TestCaptchaBrowserCloseRetriesProfileCleanup(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("f", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "marker"), []byte("remove after retry"), 0o600); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	close(exited)
	var removeCalls atomic.Int32
	controller := &chromeBrowserController{
		profileRoot: root,
		profileDir:  profileDir,
		exited:      exited,
		closeDevTools: func(context.Context, string) error {
			return errors.New("DevTools unavailable")
		},
		removeProfile: func(profileRoot, profileDir string) error {
			if removeCalls.Add(1) == 1 {
				return errors.New("temporary profile lock")
			}
			return removeCaptchaChromeProfile(profileRoot, profileDir)
		},
	}

	firstErr := controller.Close()
	if firstErr == nil || captchaBrowserMayBeRunning(firstErr) {
		t.Fatalf("first close error=%v, want exited profile-cleanup failure", firstErr)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "marker")); err != nil {
		t.Fatalf("profile removed despite failed first cleanup: %v", err)
	}
	if err := controller.Close(); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	if removeCalls.Load() != 2 {
		t.Fatalf("profile cleanup calls=%d, want 2", removeCalls.Load())
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("profile remains after successful retry: %v", err)
	}
}

func TestCaptchaBrowserCloseSerializesConcurrentRetries(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("1", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	close(exited)
	cleanupStarted := make(chan struct{})
	secondCleanup := make(chan struct{}, 1)
	releaseCleanup := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseCleanup:
		default:
			close(releaseCleanup)
		}
	})
	var cleanupCalls atomic.Int32
	controller := &chromeBrowserController{
		profileRoot: root,
		profileDir:  profileDir,
		exited:      exited,
		closeDevTools: func(context.Context, string) error {
			return errors.New("DevTools unavailable")
		},
		removeProfile: func(profileRoot, profileDir string) error {
			if cleanupCalls.Add(1) == 1 {
				close(cleanupStarted)
				<-releaseCleanup
				return removeCaptchaChromeProfile(profileRoot, profileDir)
			}
			secondCleanup <- struct{}{}
			return errors.New("concurrent cleanup entered")
		},
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- controller.Close() }()
	<-cleanupStarted
	secondDone := make(chan error, 1)
	go func() { secondDone <- controller.Close() }()
	select {
	case <-secondCleanup:
		t.Fatal("second Close entered profile cleanup before the first close finished")
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseCleanup)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if cleanupCalls.Load() != 1 {
		t.Fatalf("profile cleanup calls=%d, want one", cleanupCalls.Load())
	}
}

func TestCaptchaBrowserCloseRejectsSharedProfileRoot(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker")
	if err := os.WriteFile(marker, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	close(exited)
	controller := &chromeBrowserController{
		profileRoot: root,
		profileDir:  root,
		exited:      exited,
		closeDevTools: func(context.Context, string) error {
			return errors.New("DevToolsActivePort unavailable")
		},
	}
	closeErr := controller.Close()
	if closeErr == nil {
		t.Fatal("shared profile root removal was not rejected")
	}
	if captchaBrowserMayBeRunning(closeErr) {
		t.Fatalf("profile-only cleanup error was misclassified as a live process: %v", closeErr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("shared profile root contents were removed: %v", err)
	}
}

func TestCaptchaBrowserDevToolsCloseUsesLoopbackBrowserEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	command := make(chan map[string]any, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/devtools/browser/owned", func(w http.ResponseWriter, r *http.Request) {
		conn, upgradeErr := upgrader.Upgrade(w, r, nil)
		if upgradeErr != nil {
			return
		}
		defer conn.Close()
		var got map[string]any
		if readErr := conn.ReadJSON(&got); readErr == nil {
			command <- got
		}
	})
	server := &http.Server{Handler: mux}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serveDone)
	}()
	defer func() {
		_ = server.Close()
		<-serveDone
	}()

	profileDir := t.TempDir()
	port := listener.Addr().(*net.TCPAddr).Port
	activePort := fmt.Sprintf("%d\n/devtools/browser/owned\n", port)
	if err := os.WriteFile(filepath.Join(profileDir, "DevToolsActivePort"), []byte(activePort), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := closeChromeViaDevTools(ctx, profileDir); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-command:
		if got["method"] != "Browser.close" || got["id"] != float64(1) {
			t.Fatalf("DevTools command=%v", got)
		}
	case <-ctx.Done():
		t.Fatal("Browser.close command was not received")
	}
	if endpoint, err := parseChromeDevToolsActivePort([]byte("9222\nws://attacker.example/devtools/browser/x\n")); err == nil {
		t.Fatalf("accepted non-loopback DevTools endpoint %q", endpoint)
	}
}

func TestChromeLaunchHelperProcess(t *testing.T) {
	switch os.Getenv("VALORANT_CHROME_LAUNCH_HELPER") {
	case "fail":
		os.Exit(23)
	case "1":
		time.Sleep(2 * time.Second)
	case "pipe":
		pipe := newChromeDevToolsPipe(os.NewFile(3, "devtools-command"), os.NewFile(4, "devtools-response"))
		defer pipe.Close()
		for {
			var command struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if err := pipe.ReadJSON(&command); err != nil {
				os.Exit(24)
			}
			if command.Method == "Browser.close" {
				return
			}
			if err := pipe.WriteJSON(map[string]any{
				"id":     command.ID,
				"result": map[string]any{"product": "private-pipe-test"},
			}); err != nil {
				os.Exit(25)
			}
		}
	default:
		return
	}
}
