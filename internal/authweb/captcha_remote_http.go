package authweb

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
)

const (
	remoteCaptchaViewerCookieName = "__Secure-valorant-captcha-viewer"
	remoteCaptchaViewerCookiePath = "/api/auth/captcha/remote"
)

func (s *Server) registerRemoteCaptchaHTTPRoutes() {
	s.mux.HandleFunc("GET /captcha/remote", s.handleRemoteCaptchaViewer)
	s.mux.HandleFunc("POST /api/auth/captcha/remote/redeem", s.handleRemoteCaptchaRedeem)
	s.mux.HandleFunc("POST /api/auth/captcha/remote/cancel", s.handleRemoteCaptchaCancel)
}

func (s *Server) remoteCaptchaPublicOrigin() (origin, host string, ok bool) {
	parsed, err := url.Parse(s.authBaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", false
	}
	return parsed.Scheme + "://" + parsed.Host, parsed.Host, true
}

func (s *Server) allowRemoteCaptchaRequest(r *http.Request, requireOrigin bool) bool {
	if s.captchaBrowserMode != netutil.CaptchaBrowserRemote {
		return false
	}
	origin, host, ok := s.remoteCaptchaPublicOrigin()
	if !ok || r.Host != host {
		return false
	}
	return !requireOrigin || r.Header.Get("Origin") == origin
}

func setRemoteCaptchaSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func remoteCaptchaInlineHash(content string) string {
	digest := sha256.Sum256([]byte(content))
	return "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
}

func (s *Server) handleRemoteCaptchaViewer(w http.ResponseWriter, r *http.Request) {
	setRemoteCaptchaSecurityHeaders(w)
	if !s.allowRemoteCaptchaRequest(r, false) {
		http.Error(w, "remote captcha unavailable", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Permissions-Policy", "camera=(), display-capture=(), geolocation=(), microphone=(), payment=(), usb=()")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"script-src " + remoteCaptchaInlineHash(remoteCaptchaViewerScript),
		"style-src " + remoteCaptchaInlineHash(remoteCaptchaViewerStyle),
		"img-src 'self' blob: data:",
		"connect-src 'self'",
		"font-src 'none'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
		"manifest-src 'none'",
		"worker-src 'none'",
	}, "; "))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, remoteCaptchaViewerHTML)
}

func (s *Server) handleRemoteCaptchaRedeem(w http.ResponseWriter, r *http.Request) {
	setRemoteCaptchaSecurityHeaders(w)
	if !s.allowRemoteCaptchaRequest(r, true) {
		writeRemoteCaptchaError(w, http.StatusForbidden)
		return
	}
	var request struct {
		Token string `json:"token"`
	}
	if status := decodeRemoteCaptchaJSON(w, r, &request); status != 0 {
		writeRemoteCaptchaError(w, status)
		return
	}
	_, rawSession, err := s.redeemRemoteCaptchaGrant(request.Token)
	if err != nil {
		writeRemoteCaptchaError(w, http.StatusUnauthorized)
		return
	}
	viewer, err := s.lookupRemoteCaptchaViewer(rawSession)
	if err != nil {
		writeRemoteCaptchaError(w, http.StatusUnauthorized)
		return
	}
	maxAge := int(viewer.expiresAt.Sub(s.remoteCaptchaHooks().now()).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     remoteCaptchaViewerCookieName,
		Value:    rawSession,
		Path:     remoteCaptchaViewerCookiePath,
		Expires:  viewer.expiresAt,
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoteCaptchaCancel(w http.ResponseWriter, r *http.Request) {
	setRemoteCaptchaSecurityHeaders(w)
	if !s.allowRemoteCaptchaRequest(r, true) {
		writeRemoteCaptchaError(w, http.StatusForbidden)
		return
	}
	var request struct{}
	if status := decodeRemoteCaptchaJSON(w, r, &request); status != 0 {
		writeRemoteCaptchaError(w, status)
		return
	}
	cookie, err := r.Cookie(remoteCaptchaViewerCookieName)
	if err != nil || s.cancelRemoteCaptchaViewer(cookie.Value) != nil {
		writeRemoteCaptchaError(w, http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     remoteCaptchaViewerCookieName,
		Value:    "",
		Path:     remoteCaptchaViewerCookiePath,
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func decodeRemoteCaptchaJSON(w http.ResponseWriter, r *http.Request, destination any) int {
	r.Body = http.MaxBytesReader(w, r.Body, captchaSubmitBodyLimit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return http.StatusRequestEntityTooLarge
		}
		return http.StatusBadRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return http.StatusBadRequest
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return http.StatusBadRequest
	}
	return 0
}

func writeRemoteCaptchaError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"ok":false,"error":"expired or invalid"}`)
}

const remoteCaptchaViewerStyle = `
:root{color-scheme:dark;font-family:system-ui,sans-serif;background:#090b10;color:#f4f6fb}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center}
main{width:min(100vw,1280px);padding:16px;display:grid;gap:12px}header{display:flex;align-items:center;justify-content:space-between;gap:12px}
#status{margin:0;font-size:.95rem}button{border:1px solid #596275;border-radius:8px;background:#202634;color:inherit;padding:8px 14px;cursor:pointer}
canvas{display:block;width:100%;height:auto;aspect-ratio:1280/900;background:#111722;border-radius:10px;touch-action:none;outline:none}
`

const remoteCaptchaViewerScript = `
(()=>{"use strict";
const canvas=document.getElementById("viewer"),status=document.getElementById("status"),cancel=document.getElementById("cancel"),context=canvas.getContext("2d");
let grant=location.hash.startsWith("#")?location.hash.slice(1):"",socket=null;
history.replaceState(null,"",location.pathname);
const setStatus=value=>{status.textContent=value;};
const send=value=>{if(socket&&socket.readyState===WebSocket.OPEN)socket.send(JSON.stringify(value));};
const point=event=>{const bounds=canvas.getBoundingClientRect();return{x:(event.clientX-bounds.left)*canvas.width/bounds.width,y:(event.clientY-bounds.top)*canvas.height/bounds.height,width:bounds.width,height:bounds.height};};
const pointer=(phase,event)=>{if(event.button!==0&&phase!=="move")return;event.preventDefault();if(phase==="down")canvas.setPointerCapture(event.pointerId);send({type:"pointer",phase,...point(event),button:0});};
canvas.addEventListener("pointermove",event=>pointer("move",event));
canvas.addEventListener("pointerdown",event=>pointer("down",event));
canvas.addEventListener("pointerup",event=>pointer("up",event));
canvas.addEventListener("wheel",event=>{event.preventDefault();send({type:"wheel",deltaY:event.deltaY,...point(event)});},{passive:false});
cancel.addEventListener("click",async()=>{cancel.disabled=true;await fetch("/api/auth/captcha/remote/cancel",{method:"POST",headers:{"Content-Type":"application/json"},body:"{}"}).catch(()=>{});if(socket)socket.close();setStatus("Cancelled");});
const start=async()=>{if(!grant){setStatus("This CAPTCHA link is invalid or expired.");return;}const response=await fetch("/api/auth/captcha/remote/redeem",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({token:grant})}).catch(()=>null);grant="";if(!response||!response.ok){setStatus("This CAPTCHA link is invalid or expired.");return;}setStatus("Connecting…");const protocol=location.protocol==="https:"?"wss:":"ws:";socket=new WebSocket(protocol+"//"+location.host+"/api/auth/captcha/remote/ws");socket.binaryType="blob";socket.addEventListener("open",()=>{setStatus("Connected");send({type:"viewport",width:canvas.clientWidth,height:canvas.clientHeight});});socket.addEventListener("message",async event=>{if(typeof event.data==="string"){const message=JSON.parse(event.data);if(message.status)setStatus(message.status);return;}const bitmap=await createImageBitmap(event.data);canvas.width=bitmap.width;canvas.height=bitmap.height;context.drawImage(bitmap,0,0);bitmap.close();});socket.addEventListener("close",()=>setStatus("Disconnected"));socket.addEventListener("error",()=>setStatus("Connection failed"));};
start();
})();
`

const remoteCaptchaViewerHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Remote CAPTCHA</title><style>` + remoteCaptchaViewerStyle + `</style></head>
<body><main><header><p id="status" role="status" aria-live="polite">Preparing secure viewer…</p><button id="cancel" type="button">Cancel</button></header><canvas id="viewer" width="1280" height="900" aria-label="Remote Riot CAPTCHA viewer"></canvas></main><script>` + remoteCaptchaViewerScript + `</script></body></html>`
