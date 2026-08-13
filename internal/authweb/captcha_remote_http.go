package authweb

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
)

const (
	remoteCaptchaViewerCookieName     = "__Secure-valorant-captcha-viewer"
	remoteCaptchaViewerCookieNameHTTP = "valorant-captcha-viewer"
	remoteCaptchaViewerCookiePath     = "/api/auth/captcha/remote"
)

func (s *Server) remoteCaptchaViewerCookieSettings() (name string, secure bool) {
	if strings.HasPrefix(s.remoteCaptchaOrigin, "http://") {
		return remoteCaptchaViewerCookieNameHTTP, false
	}
	return remoteCaptchaViewerCookieName, true
}

func (s *Server) remoteCaptchaViewerCookieName() string {
	name, _ := s.remoteCaptchaViewerCookieSettings()
	return name
}

func (s *Server) newRemoteCaptchaViewerCookie(value string, expires time.Time, maxAge int) *http.Cookie {
	name, secure := s.remoteCaptchaViewerCookieSettings()
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     remoteCaptchaViewerCookiePath,
		Expires:  expires,
		MaxAge:   maxAge,
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

func (s *Server) registerRemoteCaptchaHTTPRoutes() {
	s.mux.HandleFunc("GET /captcha/remote", s.handleRemoteCaptchaViewer)
	s.mux.HandleFunc("POST /api/auth/captcha/remote/redeem", s.handleRemoteCaptchaRedeem)
	s.mux.HandleFunc("GET /api/auth/captcha/remote/ws", s.handleRemoteCaptchaWebSocket)
	s.mux.HandleFunc("POST /api/auth/captcha/remote/cancel", s.handleRemoteCaptchaCancel)
}

func (s *Server) remoteCaptchaPublicOrigin() (origin, host string, ok bool) {
	if s.remoteCaptchaOrigin == "" || s.remoteCaptchaHost == "" {
		return "", "", false
	}
	return s.remoteCaptchaOrigin, s.remoteCaptchaHost, true
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
	http.SetCookie(w, s.newRemoteCaptchaViewerCookie(rawSession, viewer.expiresAt, maxAge))
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
	cookie, err := r.Cookie(s.remoteCaptchaViewerCookieName())
	if err != nil || s.cancelRemoteCaptchaViewer(cookie.Value) != nil {
		writeRemoteCaptchaError(w, http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, s.newRemoteCaptchaViewerCookie("", time.Unix(1, 0), -1))
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
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return http.StatusBadRequest
	}
	duplicate, err := remoteCaptchaJSONHasDuplicateRootKey(trimmed)
	if err != nil || duplicate {
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

func remoteCaptchaJSONHasDuplicateRootKey(body []byte) (bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return false, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return false, errors.New("remote captcha JSON must be an object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return false, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return false, errors.New("remote captcha JSON key must be a string")
		}
		if _, exists := seen[key]; exists {
			return true, nil
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return false, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return false, errors.New("remote captcha JSON contains trailing data")
	}
	return false, nil
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
canvas{display:block;width:100%;height:auto;aspect-ratio:auto;background:#111722;border-radius:10px;touch-action:none;outline:none}
`

const remoteCaptchaViewerScript = `
(()=>{"use strict";
const canvas=document.getElementById("viewer"),status=document.getElementById("status"),cancel=document.getElementById("cancel"),context=canvas.getContext("2d");
const reconnectWindowMs=60000,reconnectDelayMs=1000;
const pointerMoveIntervalMs=1000/60;
let grant=location.hash.startsWith("#")?location.hash.slice(1):"",socket=null,reconnectDeadline=0,reconnectTimer=0,stopped=false;
let pendingPointerMove=null,pointerMoveTimer=0,lastPointerMoveAt=0,pointerActive=false;
let decodingFrame=false,pendingFrame=null,pendingFrameMetadata=null,displayedFrame=null,latestReceivedGeneration=0;
history.replaceState(null,"",location.pathname);
const setStatus=value=>{status.textContent=value;};
const send=value=>{if(socket&&socket.readyState===WebSocket.OPEN)socket.send(JSON.stringify(value));};
const point=event=>{if(!displayedFrame)return null;const bounds=canvas.getBoundingClientRect();return{x:(event.clientX-bounds.left)*canvas.width/bounds.width,y:(event.clientY-bounds.top)*canvas.height/bounds.height,width:canvas.width,height:canvas.height,generation:displayedFrame.generation};};
const discardPointerMove=()=>{clearTimeout(pointerMoveTimer);pointerMoveTimer=0;pendingPointerMove=null;};
const pointer=(phase,event)=>{if(phase!=="up"&&event.button!==0)return;if(phase==="up"&&!pointerActive)return;event.preventDefault();if(phase==="down")discardPointerMove();else flushPointerMove();const mapped=point(event);if(!mapped)return;if(phase==="down"){pointerActive=true;canvas.setPointerCapture(event.pointerId)}else pointerActive=false;send({type:"pointer",phase,...mapped,button:0});};
const flushPointerMove=()=>{pointerMoveTimer=0;if(!pendingPointerMove)return;lastPointerMoveAt=performance.now();const message=pendingPointerMove;pendingPointerMove=null;send(message);};
const queuePointerMove=event=>{event.preventDefault();const mapped=point(event);if(!mapped)return;pendingPointerMove={type:"pointer",phase:"move",...mapped,button:0};if(pointerMoveTimer)return;const wait=Math.max(0,pointerMoveIntervalMs-(performance.now()-lastPointerMoveAt));pointerMoveTimer=setTimeout(flushPointerMove,wait);};
canvas.addEventListener("pointermove",queuePointerMove);
canvas.addEventListener("pointerdown",event=>pointer("down",event));
canvas.addEventListener("pointerup",event=>pointer("up",event));
canvas.addEventListener("pointercancel",event=>pointer("up",event));
canvas.addEventListener("lostpointercapture",event=>pointer("up",event));
canvas.addEventListener("wheel",event=>{event.preventDefault();const mapped=point(event);if(mapped)send({type:"wheel",deltaY:event.deltaY,...mapped});},{passive:false});
const scheduleReconnect=()=>{if(stopped)return;if(!reconnectDeadline)reconnectDeadline=Date.now()+reconnectWindowMs;if(Date.now()+reconnectDelayMs>=reconnectDeadline){stopped=true;cancel.disabled=true;setStatus("This CAPTCHA session is no longer available.");return;}setStatus("Reconnecting…");clearTimeout(reconnectTimer);reconnectTimer=setTimeout(connect,reconnectDelayMs);};
const decodeFrame=async frame=>{if(decodingFrame){pendingFrame=frame;return;}decodingFrame=true;let next=frame;try{while(next){const bitmap=await createImageBitmap(next.blob);if(next.owner===socket&&next.metadata.generation===latestReceivedGeneration&&bitmap.width===next.metadata.width&&bitmap.height===next.metadata.height){discardPointerMove();canvas.width=bitmap.width;canvas.height=bitmap.height;context.drawImage(bitmap,0,0);displayedFrame=next.metadata;send({type:"frameAck",generation:next.metadata.generation,width:bitmap.width,height:bitmap.height});}bitmap.close();next=pendingFrame;pendingFrame=null;}}catch(_){setStatus("Frame decode interrupted");}finally{decodingFrame=false;if(pendingFrame){const latest=pendingFrame;pendingFrame=null;decodeFrame(latest);}}};
const connect=()=>{if(stopped)return;setStatus("Connecting…");discardPointerMove();pointerActive=false;displayedFrame=null;pendingFrameMetadata=null;latestReceivedGeneration=0;const protocol=location.protocol==="https:"?"wss:":"ws:";socket=new WebSocket(protocol+"//"+location.host+"/api/auth/captcha/remote/ws");socket.binaryType="blob";socket.addEventListener("open",()=>{reconnectDeadline=0;setStatus("Connected");});socket.addEventListener("message",event=>{if(typeof event.data==="string"){const message=JSON.parse(event.data);if(message.type==="frame"&&Number.isSafeInteger(message.generation)&&message.generation>0&&Number.isSafeInteger(message.width)&&message.width>0&&Number.isSafeInteger(message.height)&&message.height>0){pendingFrameMetadata=message;latestReceivedGeneration=message.generation;discardPointerMove();}else if(message.status)setStatus(message.status);return;}if(!pendingFrameMetadata)return;const metadata=pendingFrameMetadata;pendingFrameMetadata=null;decodeFrame({blob:event.data,metadata,owner:socket});});socket.addEventListener("close",event=>{socket=null;pointerActive=false;displayedFrame=null;discardPointerMove();if(stopped)return;if(event.code===1000){stopped=true;cancel.disabled=true;setStatus("CAPTCHA session finished.");return;}scheduleReconnect();});socket.addEventListener("error",()=>setStatus("Connection interrupted"));};
cancel.addEventListener("click",async()=>{stopped=true;clearTimeout(reconnectTimer);discardPointerMove();cancel.disabled=true;await fetch("/api/auth/captcha/remote/cancel",{method:"POST",headers:{"Content-Type":"application/json"},body:"{}"}).catch(()=>{});if(socket)socket.close();setStatus("Cancelled");});
const start=async()=>{if(grant){const response=await fetch("/api/auth/captcha/remote/redeem",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({token:grant})}).catch(()=>null);grant="";if(!response||!response.ok){stopped=true;setStatus("This CAPTCHA link is invalid or expired.");return;}}cancel.disabled=false;connect();};
start();
})();
`

const remoteCaptchaViewerHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Remote CAPTCHA</title><style>` + remoteCaptchaViewerStyle + `</style></head>
<body><main><header><p id="status" role="status" aria-live="polite">Preparing secure viewer…</p><button id="cancel" type="button" disabled>Cancel</button></header><canvas id="viewer" width="1280" height="900" aria-label="Remote Riot CAPTCHA viewer"></canvas></main><script>` + remoteCaptchaViewerScript + `</script></body></html>`
