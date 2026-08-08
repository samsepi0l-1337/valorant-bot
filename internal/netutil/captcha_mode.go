package netutil

import (
	"fmt"
	"net/url"
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
	origin, err := url.Parse(strings.TrimSpace(authBaseURL))
	if err != nil {
		return fmt.Errorf("CAPTCHA_BROWSER_MODE=remote requires AUTH_BASE_URL to be an HTTPS origin: %w", err)
	}
	if !strings.EqualFold(origin.Scheme, "https") || origin.Host == "" || origin.Hostname() == "" || origin.Opaque != "" ||
		origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" ||
		(origin.Path != "" && origin.Path != "/") || (origin.RawPath != "" && origin.RawPath != "/") {
		return fmt.Errorf("CAPTCHA_BROWSER_MODE=remote requires AUTH_BASE_URL to be an absolute HTTPS origin without user-info, query, fragment, or path")
	}
	return nil
}
