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
		if _, err := CanonicalRemoteCaptchaOrigin(authBaseURL); err != nil {
			return "", err
		}
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported CAPTCHA_BROWSER_MODE %q (want local, remote, or disabled)", rawMode)
	}
}

// CanonicalRemoteCaptchaOrigin validates and serializes an HTTPS origin the
// same way a browser supplies Host and Origin: lowercase DNS/scheme, canonical
// IP spelling, no root slash, and no explicit default HTTPS port.
func CanonicalRemoteCaptchaOrigin(authBaseURL string) (string, error) {
	if err := validateRemoteCaptchaOrigin(authBaseURL); err != nil {
		return "", err
	}
	origin, err := url.Parse(authBaseURL)
	if err != nil {
		return "", fmt.Errorf("parse remote CAPTCHA origin: %w", err)
	}
	hostname := origin.Hostname()
	canonicalHost := strings.ToLower(hostname)
	if address, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		canonicalHost = address.String()
		if address.Is6() {
			canonicalHost = "[" + canonicalHost + "]"
		}
	}
	if port := origin.Port(); port != "" {
		portNumber, _ := strconv.Atoi(port) // validated above
		if portNumber != 443 {
			canonicalHost += ":" + strconv.Itoa(portNumber)
		}
	}
	return "https://" + canonicalHost, nil
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
		if err != nil || !address.Is6() || address.Zone() != "" {
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
	if looksLikeNoncanonicalIPv4(hostname) {
		return fmt.Errorf("noncanonical IPv4 host")
	}
	if !validRemoteCaptchaDNSName(hostname) {
		return fmt.Errorf("malformed DNS host")
	}
	return nil
}

func looksLikeNoncanonicalIPv4(hostname string) bool {
	hostname = strings.TrimSuffix(hostname, ".")
	parts := strings.Split(hostname, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		digits := part
		base := byte(10)
		if strings.HasPrefix(part, "0x") || strings.HasPrefix(part, "0X") {
			digits = part[2:]
			base = 16
		}
		if digits == "" {
			return false
		}
		for index := 0; index < len(digits); index++ {
			char := digits[index]
			if char >= '0' && char <= '9' {
				continue
			}
			if base == 16 && ((char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
				continue
			}
			return false
		}
	}
	return true
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
