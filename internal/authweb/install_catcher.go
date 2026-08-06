package authweb

// installCatcherScript is served at GET /install-catcher.sh.
// Usage: curl -fsSL "$AUTH_BASE/install-catcher.sh" | sudo bash -s -- "$AUTH_BASE"
//
// One fmt verb: default FORWARD URL (%s).
const installCatcherScript = `#!/usr/bin/env bash
# Valorant-bot localhost auth catcher (Riot requires http://localhost/redirect on :80).
set -euo pipefail
FORWARD="${1:-%s}"
FORWARD="$(printf '%%s' "$FORWARD" | sed 's:/*$::')"
export FORWARD
echo "authcatcher → $FORWARD (listening on 127.0.0.1:80)"
exec python3 - <<'PY'
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler
import json, os

FORWARD = os.environ["FORWARD"].rstrip("/")
CALLBACK = json.dumps(FORWARD + "/api/auth/callback")
HTML = (
    "<!DOCTYPE html><html lang=ko><head><meta charset=utf-8><title>연동 중</title></head><body>"
    "<p id=m>봇으로 전송 중…</p><script>(function(){"
    f"var c={CALLBACK};"
    "var h=(location.hash||'').replace(/^#/,'');"
    "var p=new URLSearchParams(h);"
    "if(!p.get('access_token')){document.getElementById('m').textContent='token 없음';return;}"
    "var b=new URLSearchParams();"
    "b.set('state',p.get('state')||'');b.set('redirect_url',location.href);"
    "fetch(c,{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},"
    "body:b.toString(),mode:'cors'})"
    ".then(r=>r.text().then(t=>{if(!r.ok)throw new Error(t||r.status);"
    "document.open();document.write(t);document.close();}))"
    ".catch(e=>{document.getElementById('m').textContent='실패: '+e.message;});"
    "})();</script></body></html>"
)

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/catcher-ping":
            self.send_response(200)
            self.send_header("Access-Control-Allow-Origin", "*")
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(b"ok")
            return
        if path == "/redirect":
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            self.wfile.write(HTML.encode())
            return
        self.send_error(404)

print("listening http://127.0.0.1:80/redirect →", FORWARD, flush=True)
ThreadingHTTPServer(("127.0.0.1", 80), H).serve_forever()
PY
`
