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
// requirements. Remote viewers require an HTTPS origin or a private/local HTTP origin.
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

// CanonicalRemoteCaptchaOrigin validates and serializes an origin the same way
// a browser supplies Host and Origin: lowercase DNS/scheme, canonical IP
// spelling, no root slash, and no explicit default port (443 for HTTPS, 80 for HTTP).
func CanonicalRemoteCaptchaOrigin(authBaseURL string) (string, error) {
	if err := validateRemoteCaptchaOrigin(authBaseURL); err != nil {
		return "", err
	}
	origin, err := url.Parse(authBaseURL)
	if err != nil {
		return "", fmt.Errorf("parse remote CAPTCHA origin: %w", err)
	}
	scheme := strings.ToLower(origin.Scheme)
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
		defaultPort := 443
		if scheme == "http" {
			defaultPort = 80
		}
		if portNumber != defaultPort {
			canonicalHost += ":" + strconv.Itoa(portNumber)
		}
	}
	return scheme + "://" + canonicalHost, nil
}

func validateRemoteCaptchaOrigin(authBaseURL string) error {
	if authBaseURL == "" || authBaseURL != strings.TrimSpace(authBaseURL) {
		return remoteCaptchaOriginError()
	}
	origin, err := url.Parse(authBaseURL)
	if err != nil {
		return fmt.Errorf("CAPTCHA_BROWSER_MODE=remote requires AUTH_BASE_URL to be an HTTPS origin or a private/local HTTP origin: %w", err)
	}
	scheme := strings.ToLower(origin.Scheme)
	if (scheme != "https" && scheme != "http") || origin.Host == "" || origin.Hostname() == "" || origin.Opaque != "" ||
		origin.User != nil || strings.Contains(authBaseURL, "?") || strings.Contains(authBaseURL, "#") ||
		(origin.Path != "" && origin.Path != "/") || (origin.RawPath != "" && origin.RawPath != "/") {
		return remoteCaptchaOriginError()
	}
	if err := validateRemoteCaptchaHost(origin.Host); err != nil {
		return fmt.Errorf("%w: %v", remoteCaptchaOriginError(), err)
	}
	if scheme == "http" && !allowedRemoteCaptchaHTTPHost(origin.Hostname()) {
		return remoteCaptchaOriginError()
	}
	return nil
}

func allowedRemoteCaptchaHTTPHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	if address, err := netip.ParseAddr(hostname); err == nil {
		return address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast()
	}
	if !validRemoteCaptchaDNSName(hostname) {
		return false
	}
	lower := strings.ToLower(hostname)
	return strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal")
}

func remoteCaptchaOriginError() error {
	return fmt.Errorf("CAPTCHA_BROWSER_MODE=remote requires AUTH_BASE_URL to be an absolute HTTPS origin or a private/local HTTP origin without user-info, query, fragment, or path")
}

// ValidateRemoteCaptchaBind checks that AUTH_BIND_ADDRESS can serve a remote
// CAPTCHA origin. LAN IP and .local/.internal origins must not bind loopback.
func ValidateRemoteCaptchaBind(authBaseURL, bindAddress string) error {
	origin, err := url.Parse(authBaseURL)
	if err != nil || origin.Hostname() == "" {
		return remoteCaptchaOriginError()
	}
	hostname := origin.Hostname()
	if !remoteCaptchaOriginRequiresNonLoopbackBind(hostname) {
		return nil
	}
	if isLoopbackAuthBind(bindAddress) {
		return fmt.Errorf("CAPTCHA_BROWSER_MODE=remote with a LAN AUTH_BASE_URL requires AUTH_BIND_ADDRESS to be 0.0.0.0, ::, or the origin IP, not loopback")
	}
	if bindAddress == "0.0.0.0" || bindAddress == "::" {
		return nil
	}
	originAddr, originErr := netip.ParseAddr(hostname)
	bindAddr, bindErr := netip.ParseAddr(bindAddress)
	if originErr == nil && bindErr == nil && originAddr == bindAddr {
		return nil
	}
	return fmt.Errorf("CAPTCHA_BROWSER_MODE=remote with a LAN AUTH_BASE_URL requires AUTH_BIND_ADDRESS to be 0.0.0.0, ::, or the origin IP, not loopback")
}

func remoteCaptchaOriginRequiresNonLoopbackBind(hostname string) bool {
	lower := strings.ToLower(hostname)
	if strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return true
	}
	address, err := netip.ParseAddr(hostname)
	if err != nil {
		return false
	}
	if address.IsLoopback() {
		return false
	}
	return address.IsPrivate() || address.IsLinkLocalUnicast()
}

func isLoopbackAuthBind(bindAddress string) bool {
	if strings.EqualFold(bindAddress, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(bindAddress)
	return err == nil && address.IsLoopback()
}

func validateRemoteCaptchaHost(host string) error {
	if strings.HasPrefix(host, "[") {
		close := strings.IndexByte(host, ']')
		if close <= 1 {
			return fmt.Errorf("malformed bracketed IPv6 host")
		}
		address, err := netip.ParseAddr(host[1:close])
		if err != nil || !address.Is6() || address.Is4In6() || address.Zone() != "" {
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
