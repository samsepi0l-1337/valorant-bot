package authweb

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RiotCaptchaHost is the hostname used by Riot's current authenticator UI.
// Tokens must be minted on this host (not trycloudflare.com or the legacy
// auth.riotgames.com origin). Prefer HTTPS port 443 so the origin exactly
// matches the live Riot login page.
const RiotCaptchaHost = "authenticate.riotgames.com"

const defaultCaptchaTLSPort = 8443
const preferredCaptchaTLSPort = 443

// skipCaptchaTLSWait is set by unit tests that do not start a TLS listener.
var skipCaptchaTLSWait bool

// StartCaptchaTLS serves the captcha widget over HTTPS on 127.0.0.1 so a local
// Chrome instance can open https://authenticate.riotgames.com/captcha/widget
// with --host-resolver-rules mapping that name to loopback.
// When running as root (sudo), prefers port 443 so the origin matches Riot/CapMonster.
func (s *Server) StartCaptchaTLS(port int, dataDir string) error {
	if dataDir == "" {
		dataDir = "./data"
	}
	if port <= 0 {
		s.mu.Lock()
		configuredPort := s.captchaTLSConfiguredPort
		s.mu.Unlock()
		if configuredPort > 0 {
			port = configuredPort
		}
	}
	if port > 0 {
		return s.listenCaptchaTLS(port, dataDir)
	}
	if os.Geteuid() == 0 {
		if err := s.listenCaptchaTLS(preferredCaptchaTLSPort, dataDir); err == nil {
			return nil
		} else {
			log.Printf("captcha tls: :%d unavailable (%v); using :%d", preferredCaptchaTLSPort, err, defaultCaptchaTLSPort)
		}
	}
	return s.listenCaptchaTLS(defaultCaptchaTLSPort, dataDir)
}

func (s *Server) listenCaptchaTLS(port int, dataDir string) error {
	certFile, keyFile, err := ensureCaptchaTLSFiles(dataDir)
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	tlsLn := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})

	srv := &http.Server{
		Handler:  s.captchaHandler(),
		ErrorLog: quietTLSLogger(),
	}
	done := make(chan struct{})
	s.mu.Lock()
	if s.captchaTLSListener != nil {
		s.mu.Unlock()
		_ = tlsLn.Close()
		return fmt.Errorf("captcha tls already started")
	}
	s.captchaTLSPort = ln.Addr().(*net.TCPAddr).Port
	s.captchaTLSServer = srv
	s.captchaTLSListener = tlsLn
	s.captchaTLSServeErr = nil
	s.captchaTLSDone = done
	s.mu.Unlock()
	go func() {
		defer close(done)
		log.Printf("captcha tls: %s (local Chrome host-map)", s.captchaWidgetURL("{state}"))
		serveErr := srv.Serve(tlsLn)
		s.mu.Lock()
		if s.captchaTLSServer == srv {
			s.captchaTLSServeErr = serveErr
			s.captchaTLSListener = nil
		}
		s.mu.Unlock()
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("captcha tls: %v", serveErr)
		}
	}()
	return nil
}

// quietTLSLogger drops Chrome's common aborted handshake probes (EOF / "connection reset")
// which are noisy but usually harmless when the captcha page still loads.
func quietTLSLogger() *log.Logger {
	return log.New(&tlsLogFilter{out: log.Writer()}, "", log.LstdFlags)
}

type tlsLogFilter struct {
	out io.Writer
}

func (f *tlsLogFilter) Write(p []byte) (int, error) {
	msg := string(p)
	if strings.Contains(msg, "TLS handshake error") &&
		(strings.Contains(msg, "EOF") ||
			strings.Contains(msg, "connection reset") ||
			strings.Contains(msg, "i/o timeout")) {
		return len(p), nil
	}
	return f.out.Write(p)
}

func (s *Server) waitCaptchaTLS(timeout time.Duration) error {
	_ = timeout
	if skipCaptchaTLSWait {
		return nil
	}
	s.mu.Lock()
	port := s.captchaTLSPort
	listener := s.captchaTLSListener
	serveErr := s.captchaTLSServeErr
	s.mu.Unlock()
	if listener == nil || port <= 0 {
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("captcha TLS unavailable: %w", serveErr)
		}
		return fmt.Errorf("captcha TLS is not owned by this server; password CAPTCHA is unavailable")
	}
	return nil
}

func (s *Server) captchaWidgetURL(state string) string {
	s.mu.Lock()
	port := s.captchaTLSPort
	s.mu.Unlock()
	if port <= 0 {
		port = defaultCaptchaTLSPort
	}
	if port == 443 {
		return fmt.Sprintf("https://%s/captcha/widget?state=%s", RiotCaptchaHost, state)
	}
	return fmt.Sprintf("https://%s:%d/captcha/widget?state=%s", RiotCaptchaHost, port, state)
}

func ensureCaptchaTLSFiles(dataDir string) (certFile, keyFile string, err error) {
	dir := filepath.Join(dataDir, "captcha-tls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if captchaTLSFilesValid(certFile, keyFile, RiotCaptchaHost, time.Now()) {
		return certFile, keyFile, nil
	}
	certPEM, keyPEM, err := generateCaptchaSelfSigned(RiotCaptchaHost)
	if err != nil {
		return "", "", err
	}
	if err := writeFileAtomic(keyFile, keyPEM, 0o600); err != nil {
		return "", "", err
	}
	if err := writeFileAtomic(certFile, certPEM, 0o600); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

func captchaTLSFilesValid(certFile, keyFile, host string, now time.Time) bool {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil || len(pair.Certificate) == 0 {
		return false
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return false
	}
	return cert.VerifyHostname(host) == nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".captcha-tls-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func generateCaptchaSelfSigned(host string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Valorant Bot Captcha"},
			CommonName:   host,
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host, "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
