package valorantbot

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
)

func warnIfLoopbackAuthBase(authBaseURL string, authPort int) {
	if !isLoopbackAuthBase(authBaseURL) {
		return
	}
	hint := suggestLANAuthBase(authPort)
	log.Printf("NOTE: AUTH_BASE_URL=%s is only reachable on this machine.", authBaseURL)
	log.Printf("NOTE: /auth (Riot Mobile QR) works regardless, but the /invite link and browser fallback page need a reachable address, e.g. %s", hint)
}

// isLoopbackAuthBase reports whether AUTH_BASE_URL points at this machine's
// loopback address (same-PC login). LAN / public hosts return false.
func isLoopbackAuthBase(authBaseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(authBaseURL))
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func suggestLANAuthBase(authPort int) string {
	if authPort <= 0 {
		authPort = 8787
	}
	ip := primaryIPv4()
	if ip == "" {
		return fmt.Sprintf("http://<your-lan-ip>:%d", authPort)
	}
	return fmt.Sprintf("http://%s:%d", ip, authPort)
}

func primaryIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}
			return ip.String()
		}
	}
	return ""
}
