package authweb

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	default:
		return
	}
}
