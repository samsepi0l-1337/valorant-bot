package authweb

import (
	"fmt"
	"log"
	"net"
	"net/http"
)

// DefaultLocalhostAddr is where Riot redirects after browser login
// (client_id=riot-client → http://localhost/redirect).
const DefaultLocalhostAddr = "127.0.0.1:80"

// ListenLocalhost makes this bot process own localhost for Riot redirects.
// Serves /redirect and /catcher-ping so no separate authcatcher is required
// when the browser runs on the same machine as the bot.
func (s *Server) ListenLocalhost(addr string) error {
	if addr == "" {
		addr = DefaultLocalhostAddr
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /catcher-ping", s.handleCatcherPing)
	mux.HandleFunc("GET /redirect", s.handleRedirectCatcher)
	mux.HandleFunc("/redirect", s.handleRedirectCatcher)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	log.Printf("localhost auth: listening on http://%s/redirect (bot is localhost)", addr)
	srv := &http.Server{Handler: mux}
	return srv.Serve(ln)
}
