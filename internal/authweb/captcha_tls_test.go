package authweb

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
}

func TestCaptchaChromeUsesAuthenticateProfile(t *testing.T) {
	got := captchaChromeProfileDir("/Users/tester")
	want := filepath.Join("/Users/tester", ".cache", "valorant-bot-captcha-chrome-authenticate")
	if got != want {
		t.Fatalf("captcha Chrome profile = %q, want %q", got, want)
	}
}
