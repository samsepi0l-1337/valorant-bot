package valorantbot

import (
	"log"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
)

func warnIfLoopbackAuthBase(authBaseURL string, authPort int) {
	_ = authPort
	if !netutil.IsPrivateOrLocalAuthBaseURL(authBaseURL) {
		return
	}
	log.Printf("NOTE: AUTH_BASE_URL=%s is not reachable from Discord/mobile data.", authBaseURL)
	log.Printf("NOTE: QR and password /auth still work; only the public /invite page needs a reachable URL (optional Cloudflare Tunnel: ./scripts/pi-tunnel.sh)")
}
