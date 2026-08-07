package netutil

import (
	"net"
	"net/url"
	"strings"
)

// IsPrivateOrLocalAuthBaseURL reports AUTH_BASE_URL values that Discord users
// on another network (e.g. mobile data) cannot open. Raspberry Pi LAN IPs fall here.
func IsPrivateOrLocalAuthBaseURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return true
	}
	host := u.Hostname()
	if host == "" {
		return true
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Public hostname (e.g. trycloudflare.com, bot.example.com).
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}
