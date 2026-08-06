package main

// authcatcher listens on http://localhost/redirect (port 80) and forwards
// Riot OAuth tokens to a remote valorant-bot AUTH_BASE_URL.
//
// Usage:
//
//	sudo AUTH_BASE_URL=http://192.168.0.37:8787 authcatcher
//
// Or:
//
//	sudo ./authcatcher -forward http://192.168.0.37:8787
import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	forward := flag.String("forward", os.Getenv("AUTH_BASE_URL"), "bot AUTH_BASE_URL to POST tokens to")
	addr := flag.String("addr", "127.0.0.1:80", "listen address (Riot requires localhost:80)")
	flag.Parse()
	base := strings.TrimRight(strings.TrimSpace(*forward), "/")
	if base == "" {
		log.Fatal("AUTH_BASE_URL or -forward is required (e.g. http://192.168.0.37:8787)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /catcher-ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, catcherHTML, base+"/api/auth/callback")
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, catcherHTML, base+"/api/auth/callback")
	})

	log.Printf("authcatcher listening on http://%s/redirect → %s", *addr, base)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

const catcherHTML = `<!DOCTYPE html>
<html lang="ko"><head><meta charset="utf-8"><title>연동 중…</title></head>
<body>
<p id="msg">봇 서버로 토큰 전송 중…</p>
<script>
(function(){
  var callback = %q;
  var hash = (location.hash||"").replace(/^#/,"");
  var p = new URLSearchParams(hash);
  if (!p.get("access_token")) {
    document.getElementById("msg").textContent = "access_token 없음. Discord /auth 를 다시 실행하세요.";
    return;
  }
  var body = new URLSearchParams();
  body.set("state", p.get("state")||"");
  body.set("redirect_url", location.href);
  fetch(callback, {method:"POST", headers:{"Content-Type":"application/x-www-form-urlencoded"}, body:body.toString(), mode:"cors"})
    .then(function(r){ return r.text().then(function(t){ if(!r.ok) throw new Error(t||r.status); document.open(); document.write(t); document.close(); }); })
    .catch(function(e){ document.getElementById("msg").textContent = "실패: "+e.message; });
})();
</script>
</body></html>
`
