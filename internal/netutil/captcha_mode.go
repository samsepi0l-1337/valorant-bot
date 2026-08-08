package netutil

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// CaptchaBrowserMode controls where the password-login CAPTCHA browser runs.
type CaptchaBrowserMode string

const (
	CaptchaBrowserLocal    CaptchaBrowserMode = "local"
	CaptchaBrowserRemote   CaptchaBrowserMode = "remote"
	CaptchaBrowserDisabled CaptchaBrowserMode = "disabled"
)

// NormalizeCaptchaBrowserMode validates a CAPTCHA browser mode and its origin
// requirements. Remote viewers are served only from a public HTTPS origin.
func NormalizeCaptchaBrowserMode(rawMode, authBaseURL string) (CaptchaBrowserMode, error) {
	mode := CaptchaBrowserMode(strings.ToLower(strings.TrimSpace(rawMode)))
	if mode == "" {
		return CaptchaBrowserLocal, nil
	}

	switch mode {
	case CaptchaBrowserLocal, CaptchaBrowserDisabled:
		return mode, nil
	case CaptchaBrowserRemote:
		if err := validateRemoteCaptchaOrigin(authBaseURL); err != nil {
			return "", err
		}
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported CAPTCHA_BROWSER_MODE %q (want local, remote, or disabled)", rawMode)
	}
}

func validateRemoteCaptchaOrigin(authBaseURL string) error {
	if authBaseURL == "" || authBaseURL != strings.TrimSpace(authBaseURL) {
		return remoteCaptchaOriginError()
	}
	origin, err := url.Parse(authBaseURL)
	if err != nil {
		return fmt.Errorf("CAPTCHA_BROWSER_MODE=remote requires AUTH_BASE_URL to be an HTTPS origin: %w", err)
	}
	if !strings.EqualFold(origin.Scheme, "https") || origin.Host == "" || origin.Hostname() == "" || origin.Opaque != "" ||
		origin.User != nil || strings.Contains(authBaseURL, "?") || strings.Contains(authBaseURL, "#") ||
		(origin.Path != "" && origin.Path != "/") || (origin.RawPath != "" && origin.RawPath != "/") {
		return remoteCaptchaOriginError()
	}
	if err := validateRemoteCaptchaHost(origin.Host); err != nil {
		return fmt.Errorf("%w: %v", remoteCaptchaOriginError(), err)
	}
	return nil
}

func remoteCaptchaOriginError() error {
	return fmt.Errorf("CAPTCHA_BROWSER_MODE=remote requires AUTH_BASE_URL to be an absolute HTTPS origin without user-info, query, fragment, or path")
}

func validateRemoteCaptchaHost(host string) error {
	if strings.HasPrefix(host, "[") {
		close := strings.IndexByte(host, ']')
		if close <= 1 {
			return fmt.Errorf("malformed bracketed IPv6 host")
		}
		address, err := netip.ParseAddr(host[1:close])
		if err != nil || !address.Is6() {
			return fmt.Errorf("malformed bracketed IPv6 host")
		}
		rest := host[close+1:]
		if rest == "" {
			return nil
		}
		if !strings.HasPrefix(rest, ":") {
			return fmt.Errorf("malformed bracketed IPv6 host")
		}
		return validateRemoteCaptchaPort(rest[1:])
	}

	if strings.ContainsAny(host, "[]") {
		return fmt.Errorf("malformed host")
	}
	if strings.Count(host, ":") > 1 {
		return fmt.Errorf("IPv6 hosts must use brackets")
	}
	hostname := host
	if colon := strings.IndexByte(host, ':'); colon >= 0 {
		hostname = host[:colon]
		if err := validateRemoteCaptchaPort(host[colon+1:]); err != nil {
			return err
		}
	}
	if hostname == "" {
		return fmt.Errorf("missing host")
	}
	if address, err := netip.ParseAddr(hostname); err == nil && address.Is4() {
		return nil
	}
	if !validRemoteCaptchaDNSName(hostname) {
		return fmt.Errorf("malformed DNS host")
	}
	return nil
}

func validateRemoteCaptchaPort(rawPort string) error {
	if rawPort == "" {
		return fmt.Errorf("missing port")
	}
	for _, char := range rawPort {
		if char < '0' || char > '9' {
			return fmt.Errorf("port must be numeric")
		}
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func validRemoteCaptchaDNSName(hostname string) bool {
	if strings.HasSuffix(hostname, ".") {
		hostname = strings.TrimSuffix(hostname, ".")
	}
	if hostname == "" || len(hostname) > 253 {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' {
				return false
			}
		}
	}
	return true
}
