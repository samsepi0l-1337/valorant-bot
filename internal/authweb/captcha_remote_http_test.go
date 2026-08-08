package authweb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/dosfsociety/valorant-bot/internal/netutil"
)

const remoteCaptchaTestOrigin = "https://relay.example.com"

func newRemoteCaptchaHTTPFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	grant := bytes.Repeat([]byte{0x18}, remoteCaptchaSecretBytes)
	viewer := bytes.Repeat([]byte{0x29}, remoteCaptchaSecretBytes)
	rejectedViewer := bytes.Repeat([]byte{0x3a}, remoteCaptchaSecretBytes)
	entropy := append(append(append([]byte{}, grant...), viewer...), rejectedViewer...)
	s := newRemoteCaptchaStateServer(t, entropy, time.Minute)
	remoteURL, state, err := s.BeginPasswordLogin(context.Background(), "discord-owner", "riot-user", "riot-password")
	if err != nil {
		t.Fatal(err)
	}
	return s, remoteBearerFromURL(t, remoteURL), state
}

func remoteCaptchaHTTPRequest(method, path, origin, body string) *http.Request {
	req := httptest.NewRequest(method, remoteCaptchaTestOrigin+path, strings.NewReader(body))
	req.Host = "relay.example.com"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func serveRemoteCaptchaHTTP(s *Server, req *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, req)
	return recorder
}

func TestRemoteCaptchaHTTPShellIsStaticAndHardened(t *testing.T) {
	s, bearer, _ := newRemoteCaptchaHTTPFixture(t)
	req := remoteCaptchaHTTPRequest(http.MethodGet, "/captcha/remote", "", "")
	recorder := serveRemoteCaptchaHTTP(s, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET shell status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, bearer) {
		t.Fatal("viewer HTML disclosed the remote bearer")
	}
	for _, want := range []string{"<canvas", "id=\"status\"", "id=\"cancel\"", "pointerdown", "pointermove", "pointerup", "wheel"} {
		if !strings.Contains(body, want) {
			t.Errorf("viewer HTML missing %q", want)
		}
	}
	for _, forbidden := range []string{"https://", "http://", "<input", "keydown", "keyup", "localStorage", "sessionStorage", "serviceWorker", `type:"viewport"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("viewer HTML contains forbidden capability %q", forbidden)
		}
	}
	clearIndex := strings.Index(body, "history.replaceState")
	networkIndex := strings.Index(body, "fetch(")
	if clearIndex < 0 || networkIndex < 0 || clearIndex > networkIndex {
		t.Fatal("viewer must clear location.hash before its first network request")
	}

	wantHeaders := map[string]string{
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "nosniff",
	}
	for name, want := range wantHeaders {
		if got := recorder.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	csp := recorder.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "object-src 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
	for _, forbidden := range []string{"'unsafe-inline'", "'unsafe-eval'", " *", "src=", "href="} {
		if strings.Contains(csp, forbidden) || strings.Contains(body, forbidden) {
			t.Errorf("viewer shell or CSP contains external/unsafe marker %q", forbidden)
		}
	}
	for _, element := range []string{"style", "script"} {
		inline := remoteCaptchaInlineElement(t, body, element)
		digest := sha256.Sum256([]byte(inline))
		wantHash := "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
		if !strings.Contains(csp, wantHash) {
			t.Errorf("CSP missing %s hash %s", element, wantHash)
		}
	}
}

func remoteCaptchaInlineElement(t *testing.T, body, element string) string {
	t.Helper()
	open := "<" + element + ">"
	close := "</" + element + ">"
	start := strings.Index(body, open)
	end := strings.Index(body, close)
	if start < 0 || end < start {
		t.Fatalf("viewer HTML missing inline %s", element)
	}
	return body[start+len(open) : end]
}

func TestRemoteCaptchaHTTPShellDisablesCancelUntilDelayedRedemptionCompletes(t *testing.T) {
	s, _, _ := newRemoteCaptchaHTTPFixture(t)
	recorder := serveRemoteCaptchaHTTP(s, remoteCaptchaHTTPRequest(http.MethodGet, "/captcha/remote", "", ""))
	body := recorder.Body.String()
	if !strings.Contains(body, `id="cancel" type="button" disabled`) {
		t.Fatal("cancel control is active while bearer redemption may still be pending")
	}
	redeem := strings.Index(body, `const response=await fetch("/api/auth/captcha/remote/redeem"`)
	redeemComplete := strings.Index(body, `if(!response||!response.ok)`)
	enable := strings.Index(body, `cancel.disabled=false`)
	if redeem < 0 || redeemComplete < redeem || enable < redeemComplete {
		t.Fatalf("cancel enable order redeem=%d complete=%d enable=%d", redeem, redeemComplete, enable)
	}
}

func TestRemoteCaptchaHTTPShellReloadAndTransportLossReuseViewerCookieWithoutRedeemingAgain(t *testing.T) {
	s, bearer, _ := newRemoteCaptchaHTTPFixture(t)
	cookie := redeemedRemoteCaptchaViewerCookie(t, s, bearer)

	reload := remoteCaptchaHTTPRequest(http.MethodGet, "/captcha/remote", "", "")
	reload.AddCookie(cookie)
	recorder := serveRemoteCaptchaHTTP(s, reload)
	if recorder.Code != http.StatusOK {
		t.Fatalf("cookie-authenticated reload status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`const reconnectWindowMs=60000,reconnectDelayMs=1000`,
		`const connect=()=>`,
		`if(grant){`,
		`setTimeout(connect,reconnectDelayMs)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("viewer production script missing reconnect behavior %q", want)
		}
	}
	if strings.Contains(body, `if(!grant){setStatus("This CAPTCHA link is invalid or expired.");return;}`) {
		t.Fatal("cookie-authenticated reload is rejected before attempting its WebSocket")
	}
	if got := strings.Count(body, `/api/auth/captcha/remote/redeem`); got != 1 {
		t.Fatalf("viewer redeem call sites=%d, want one grant-only redemption", got)
	}
	conditional := strings.Index(body, `if(grant){`)
	redeem := strings.Index(body, `fetch("/api/auth/captcha/remote/redeem"`)
	connect := strings.LastIndex(body, `connect();`)
	if conditional < 0 || redeem < conditional || connect < redeem {
		t.Fatalf("viewer startup order conditional=%d redeem=%d connect=%d", conditional, redeem, connect)
	}
	if strings.Contains(body, bearer) || strings.Contains(body, cookie.Value) {
		t.Fatal("cookie-authenticated reload shell disclosed an authentication secret")
	}
}

func TestRemoteCaptchaHTTPShellCoalescesPointerMoveAtSixtyHertz(t *testing.T) {
	s, _, _ := newRemoteCaptchaHTTPFixture(t)
	recorder := serveRemoteCaptchaHTTP(s, remoteCaptchaHTTPRequest(http.MethodGet, "/captcha/remote", "", ""))
	body := recorder.Body.String()
	for _, want := range []string{
		`const pointerMoveIntervalMs=1000/60`,
		`let pendingPointerMove=null,pointerMoveTimer=0,lastPointerMoveAt=0`,
		`const queuePointerMove=event=>`,
		`pendingPointerMove={type:"pointer",phase:"move"`,
		`setTimeout(flushPointerMove,wait)`,
		`canvas.addEventListener("pointermove",queuePointerMove)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("viewer production script missing 60 Hz pointer coalescing %q", want)
		}
	}
	if strings.Contains(body, `canvas.addEventListener("pointermove",event=>pointer("move",event))`) {
		t.Fatal("viewer sends each native pointermove without coalescing")
	}
}

// Mutation contract: this executes the exact shipped script, so replacing the
// reconnect callback with a no-op, changing its delay to zero, or sending every
// native pointermove makes the assertions below fail. String-presence checks do
// not prove any of those behaviors.
func TestRemoteCaptchaViewerScriptReconnectsAndCoalescesNewestPointerMove(t *testing.T) {
	redeemed := newRemoteCaptchaViewerRuntime(t, "#fragment-secret")
	assertRemoteCaptchaViewerJS(t, redeemed, `
		if (__state.historyCalls.length !== 1) throw new Error("fragment was not cleared");
		if (__state.fetchCalls.length !== 1) throw new Error("fragment grant was not redeemed exactly once");
		if (__state.sockets.length !== 1) throw new Error("redeemed viewer did not connect");
	`)

	reloaded := newRemoteCaptchaViewerRuntime(t, "")
	assertRemoteCaptchaViewerJS(t, reloaded, `
		if (__state.fetchCalls.length !== 0) throw new Error("hashless cookie reload tried to redeem a grant");
		if (__state.sockets.length !== 1) throw new Error("hashless cookie reload did not connect");
		__state.sockets[0].listeners.close({code: 1006});
		const reconnect = __state.timers.find(timer => !timer.cleared);
		if (!reconnect) throw new Error("transport close did not schedule reconnect");
		if (reconnect.delay !== 1000) throw new Error("reconnect delay must be the bounded nonzero 1000ms interval");
		reconnect.callback();
		if (__state.sockets.length !== 2) throw new Error("scheduled reconnect callback did not invoke connect");
	`)

	retryExpired := newRemoteCaptchaViewerRuntime(t, "")
	assertRemoteCaptchaViewerJS(t, retryExpired, `
		__state.sockets[0].listeners.close({code: 1006});
		const firstRetry = __state.timers.find(timer => !timer.cleared);
		if (!firstRetry) throw new Error("initial transport loss did not schedule retry");
		firstRetry.cleared = true;
		firstRetry.callback();
		if (__state.sockets.length !== 2) throw new Error("initial retry did not reconnect");
		__state.wallNow = 1000 + 60001;
		__state.sockets[1].listeners.close({code: 1006});
		if (__state.timers.some(timer => !timer.cleared)) throw new Error("retry window expiry scheduled another timer");
		if (__state.sockets.length !== 2) throw new Error("retry window expiry opened another connection");
	`)

	pointer := newRemoteCaptchaViewerRuntime(t, "")
	assertRemoteCaptchaViewerJS(t, pointer, `
		const move = __state.canvasListeners.pointermove;
		if (typeof move !== "function") throw new Error("pointermove handler missing");
		const event = clientX => ({clientX, clientY: 25, preventDefault() {}});
		__state.performanceNow = 0;
		move(event(10));
		move(event(20));
		const firstFlushes = __state.timers.filter(timer => !timer.cleared);
		if (firstFlushes.length !== 1) throw new Error("pointer moves were not coalesced behind one timer");
		if (firstFlushes[0].delay < 16) throw new Error("pointer move flush was scheduled below the 60Hz interval");
		firstFlushes[0].callback();
		if (__state.sockets[0].sent.length !== 1) throw new Error("coalesced pointer move did not send exactly once");
		if (JSON.parse(__state.sockets[0].sent[0]).x !== 20) throw new Error("coalescing did not retain the newest pointer move");
		move(event(30));
		move(event(40));
		const secondFlushes = __state.timers.filter(timer => !timer.cleared && timer !== firstFlushes[0]);
		if (secondFlushes.length !== 1) throw new Error("second pointer burst did not schedule exactly one timer");
		if (secondFlushes[0].delay < 16) throw new Error("subsequent pointer flush was scheduled below the 60Hz interval");
		secondFlushes[0].callback();
		if (__state.sockets[0].sent.length !== 2) throw new Error("second pointer burst did not send exactly once");
		if (JSON.parse(__state.sockets[0].sent[1]).x !== 40) throw new Error("second burst did not retain newest pointer move");
	`)
}

func newRemoteCaptchaViewerRuntime(t *testing.T, hash string) *goja.Runtime {
	t.Helper()
	encodedHash, err := json.Marshal(hash)
	if err != nil {
		t.Fatal(err)
	}
	runtime := goja.New()
	harness := `
		var __state = {
			canvasListeners: Object.create(null), cancelListeners: Object.create(null),
			fetchCalls: [], historyCalls: [], sockets: [], timers: [],
			performanceNow: 0, wallNow: 1000, nextTimer: 1
		};
		var __canvas = {
			width: 1280, height: 900,
			getContext: function() { return {drawImage: function() {}}; },
			addEventListener: function(name, listener) { __state.canvasListeners[name] = listener; },
			getBoundingClientRect: function() { return {left: 0, top: 0, width: 1280, height: 900}; },
			setPointerCapture: function() {}
		};
		var __status = {textContent: ""};
		var __cancel = {disabled: true, addEventListener: function(name, listener) { __state.cancelListeners[name] = listener; }};
		var document = {getElementById: function(id) { return id === "viewer" ? __canvas : id === "status" ? __status : __cancel; }};
		var location = {hash: ` + string(encodedHash) + `, pathname: "/captcha/remote", protocol: "https:", host: "relay.example.com"};
		var history = {replaceState: function() { __state.historyCalls.push(Array.from(arguments)); location.hash = ""; }};
		function WebSocket(url) {
			this.url = url; this.readyState = WebSocket.OPEN; this.binaryType = "";
			this.listeners = Object.create(null); this.sent = []; __state.sockets.push(this);
		}
		WebSocket.OPEN = 1;
		WebSocket.prototype.addEventListener = function(name, listener) { this.listeners[name] = listener; };
		WebSocket.prototype.send = function(payload) { this.sent.push(payload); };
		WebSocket.prototype.close = function() {};
		async function fetch() { __state.fetchCalls.push(Array.from(arguments)); return {ok: true}; }
		async function createImageBitmap() { return {width: 1280, height: 900, close: function() {}}; }
		var performance = {now: function() { return __state.performanceNow; }};
		var Date = {now: function() { return __state.wallNow; }};
		function setTimeout(callback, delay) {
			var timer = {id: __state.nextTimer++, callback: callback, delay: delay, cleared: false};
			__state.timers.push(timer); return timer.id;
		}
		function clearTimeout(id) {
			var timer = __state.timers.find(function(candidate) { return candidate.id === id; });
			if (timer) timer.cleared = true;
		}
	`
	if _, err := runtime.RunString(harness); err != nil {
		t.Fatalf("initialize pure-Go viewer runtime: %v", err)
	}
	if _, err := runtime.RunString(remoteCaptchaViewerScript); err != nil {
		t.Fatalf("execute shipped viewer script: %v", err)
	}
	return runtime
}

func assertRemoteCaptchaViewerJS(t *testing.T, runtime *goja.Runtime, source string) {
	t.Helper()
	if _, err := runtime.RunString(source); err != nil {
		t.Fatalf("shipped viewer behavior: %v", err)
	}
}

func TestRemoteCaptchaHTTPShellRejectsWrongHostWithoutForwardedTrust(t *testing.T) {
	s, _, _ := newRemoteCaptchaHTTPFixture(t)
	req := remoteCaptchaHTTPRequest(http.MethodGet, "/captcha/remote", "", "")
	req.Host = "attacker.example"
	req.Header.Set("X-Forwarded-Host", "relay.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := serveRemoteCaptchaHTTP(s, req).Code; got != http.StatusForbidden {
		t.Fatalf("wrong-host viewer shell status = %d, want 403", got)
	}
}

func TestRemoteCaptchaHTTPCanonicalConfiguredOriginsMatchBrowserRequests(t *testing.T) {
	for _, test := range []struct {
		name           string
		configured     string
		origin         string
		host           string
		rejectedOrigin string
		rejectedHost   string
	}{
		{name: "DNS scheme case and default port", configured: "HTTPS://Relay.Example.COM:443/", origin: "https://relay.example.com", host: "relay.example.com", rejectedOrigin: "https://Relay.Example.COM:443", rejectedHost: "Relay.Example.COM:443"},
		{name: "DNS nondefault port", configured: "https://Relay.Example.COM:8443", origin: "https://relay.example.com:8443", host: "relay.example.com:8443"},
		{name: "IPv4 default port", configured: "https://192.0.2.10:443", origin: "https://192.0.2.10", host: "192.0.2.10"},
		{name: "IPv4 nondefault port", configured: "https://192.0.2.10:8443", origin: "https://192.0.2.10:8443", host: "192.0.2.10:8443"},
		{name: "IPv6 spelling and default port", configured: "https://[2001:0DB8:0:0:0:0:0:1]:443", origin: "https://[2001:db8::1]", host: "[2001:db8::1]"},
		{name: "IPv6 nondefault port", configured: "https://[2001:0DB8::1]:8443", origin: "https://[2001:db8::1]:8443", host: "[2001:db8::1]:8443"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := New(Deps{AuthBaseURL: test.configured, CaptchaBrowserMode: netutil.CaptchaBrowserRemote})
			t.Cleanup(func() { _ = s.Close() })
			if s.authBaseURL != test.origin {
				t.Fatalf("retained auth origin = %q, want %q", s.authBaseURL, test.origin)
			}
			req := httptest.NewRequest(http.MethodPost, test.origin+"/api/auth/captcha/remote/cancel", strings.NewReader(`{}`))
			req.Host = test.host
			req.Header.Set("Origin", test.origin)
			req.Header.Set("Content-Type", "application/json")
			if got := serveRemoteCaptchaHTTP(s, req).Code; got != http.StatusUnauthorized {
				t.Fatalf("canonical browser request status = %d, want 401", got)
			}
			if test.rejectedHost != "" {
				noncanonical := httptest.NewRequest(http.MethodPost, test.origin+"/api/auth/captcha/remote/cancel", strings.NewReader(`{}`))
				noncanonical.Host = test.rejectedHost
				noncanonical.Header.Set("Origin", test.rejectedOrigin)
				noncanonical.Header.Set("Content-Type", "application/json")
				if got := serveRemoteCaptchaHTTP(s, noncanonical).Code; got != http.StatusForbidden {
					t.Fatalf("noncanonical Host/Origin status = %d, want 403", got)
				}
			}
		})
	}
}

func TestRemoteCaptchaHTTPFailsClosedForIPv4MappedIPv6Origins(t *testing.T) {
	for _, test := range []struct {
		name          string
		configured    string
		browserOrigin string
		browserHost   string
	}{
		{name: "default port", configured: "https://[::ffff:192.0.2.1]", browserOrigin: "https://[::ffff:c000:201]", browserHost: "[::ffff:c000:201]"},
		{name: "explicit nondefault port", configured: "https://[::ffff:192.0.2.1]:8443", browserOrigin: "https://[::ffff:c000:201]:8443", browserHost: "[::ffff:c000:201]:8443"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := New(Deps{AuthBaseURL: test.configured, CaptchaBrowserMode: netutil.CaptchaBrowserRemote})
			t.Cleanup(func() { _ = s.Close() })
			if s.remoteCaptchaOrigin != "" || s.remoteCaptchaHost != "" {
				t.Fatalf("server retained mapped IPv6 origin=%q host=%q", s.remoteCaptchaOrigin, s.remoteCaptchaHost)
			}
			req := httptest.NewRequest(http.MethodGet, test.browserOrigin+"/captcha/remote", nil)
			req.Host = test.browserHost
			if got := serveRemoteCaptchaHTTP(s, req).Code; got != http.StatusForbidden {
				t.Fatalf("mapped IPv6 viewer request status = %d, want 403", got)
			}
		})
	}
}

func TestRemoteCaptchaHTTPRoutesAreRemoteOnlyAndLegacyRoutesStayPrivate(t *testing.T) {
	local := New(Deps{CaptchaBrowserMode: netutil.CaptchaBrowserLocal})
	t.Cleanup(func() { _ = local.Close() })
	if got := serveRemoteCaptchaHTTP(local, remoteCaptchaHTTPRequest(http.MethodGet, "/captcha/remote", "", "")).Code; got != http.StatusNotFound {
		t.Fatalf("local mode remote shell status = %d, want 404", got)
	}

	remote, _, _ := newRemoteCaptchaHTTPFixture(t)
	for _, path := range []string{"/captcha/widget", "/api/auth/captcha/challenge"} {
		if got := serveRemoteCaptchaHTTP(remote, remoteCaptchaHTTPRequest(http.MethodGet, path, "", "")).Code; got != http.StatusNotFound {
			t.Errorf("public legacy route %s status = %d, want 404", path, got)
		}
	}
}

func TestRemoteCaptchaHTTPRedeemSetsOneOpaqueStrictCookie(t *testing.T) {
	s, bearer, state := newRemoteCaptchaHTTPFixture(t)
	body := fmt.Sprintf(`{"token":%q}`, bearer)
	recorder := serveRemoteCaptchaHTTP(s, remoteCaptchaHTTPRequest(http.MethodPost, "/api/auth/captcha/remote/redeem", remoteCaptchaTestOrigin, body))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("redeem status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("redemption cookies = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Value == bearer || cookie.Value == "" {
		t.Fatal("viewer cookie reused or omitted the fragment bearer")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(cookie.Value)
	if err != nil || len(decoded) != remoteCaptchaSecretBytes {
		t.Fatalf("opaque viewer cookie decoded=%d err=%v", len(decoded), err)
	}
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode ||
		cookie.Path != remoteCaptchaViewerCookiePath || cookie.Domain != "" {
		t.Fatalf("viewer cookie attributes = %+v", cookie)
	}
	viewer, err := s.lookupRemoteCaptchaViewer(cookie.Value)
	if err != nil || viewer.state != state || viewer.discordUserID != "discord-owner" {
		t.Fatalf("authenticated viewer = %+v, %v", viewer, err)
	}

	second := serveRemoteCaptchaHTTP(s, remoteCaptchaHTTPRequest(http.MethodPost, "/api/auth/captcha/remote/redeem", remoteCaptchaTestOrigin, body))
	if second.Code != http.StatusUnauthorized || strings.Contains(second.Body.String(), bearer) {
		t.Fatalf("reused bearer status=%d body=%q", second.Code, second.Body.String())
	}
}

func TestRemoteCaptchaHTTPRedeemRejectsUntrustedOrMalformedRequests(t *testing.T) {
	s, bearer, _ := newRemoteCaptchaHTTPFixture(t)
	validBody := fmt.Sprintf(`{"token":%q}`, bearer)

	tests := []struct {
		name       string
		method     string
		host       string
		origin     string
		body       string
		wantStatus int
	}{
		{name: "wrong host despite forwarded host", method: http.MethodPost, host: "attacker.example", origin: remoteCaptchaTestOrigin, body: validBody, wantStatus: http.StatusForbidden},
		{name: "wrong origin despite forwarded proto", method: http.MethodPost, host: "relay.example.com", origin: "https://attacker.example", body: validBody, wantStatus: http.StatusForbidden},
		{name: "missing origin", method: http.MethodPost, host: "relay.example.com", body: validBody, wantStatus: http.StatusForbidden},
		{name: "non post", method: http.MethodPut, host: "relay.example.com", origin: remoteCaptchaTestOrigin, body: validBody, wantStatus: http.StatusMethodNotAllowed},
		{name: "malformed base64", method: http.MethodPost, host: "relay.example.com", origin: remoteCaptchaTestOrigin, body: `{"token":"not/base64=="}`, wantStatus: http.StatusUnauthorized},
		{name: "null json", method: http.MethodPost, host: "relay.example.com", origin: remoteCaptchaTestOrigin, body: `null`, wantStatus: http.StatusBadRequest},
		{name: "duplicate token key", method: http.MethodPost, host: "relay.example.com", origin: remoteCaptchaTestOrigin, body: `{"token":"first","token":"second"}`, wantStatus: http.StatusBadRequest},
		{name: "unknown json", method: http.MethodPost, host: "relay.example.com", origin: remoteCaptchaTestOrigin, body: fmt.Sprintf(`{"token":%q,"owner":"discord-owner"}`, bearer), wantStatus: http.StatusBadRequest},
		{name: "oversized body", method: http.MethodPost, host: "relay.example.com", origin: remoteCaptchaTestOrigin, body: `{"token":"` + strings.Repeat("a", captchaSubmitBodyLimit) + `"}`, wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := remoteCaptchaHTTPRequest(test.method, "/api/auth/captcha/remote/redeem", test.origin, test.body)
			req.Host = test.host
			req.Header.Set("X-Forwarded-Host", "relay.example.com")
			req.Header.Set("X-Forwarded-Proto", "https")
			recorder := serveRemoteCaptchaHTTP(s, req)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body = %q", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), bearer) {
				t.Fatal("failure body disclosed bearer")
			}
		})
	}
}

func TestRemoteCaptchaHTTPRedeemRejectsExpiredWrongOwnerAndClosedState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Server, string)
	}{
		{name: "expired", mutate: func(s *Server, state string) {
			s.mu.Lock()
			pending := s.passwordPending[state]
			pending.remoteGrant.expiresAt = time.Now().Add(-time.Second)
			s.passwordPending[state] = pending
			s.mu.Unlock()
		}},
		{name: "wrong owner", mutate: func(s *Server, state string) {
			s.mu.Lock()
			pending := s.passwordPending[state]
			pending.discordUserID = "different-owner"
			s.passwordPending[state] = pending
			s.mu.Unlock()
		}},
		{name: "closed", mutate: func(s *Server, _ string) { _ = s.Close() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, bearer, state := newRemoteCaptchaHTTPFixture(t)
			test.mutate(s, state)
			body := fmt.Sprintf(`{"token":%q}`, bearer)
			recorder := serveRemoteCaptchaHTTP(s, remoteCaptchaHTTPRequest(http.MethodPost, "/api/auth/captcha/remote/redeem", remoteCaptchaTestOrigin, body))
			if recorder.Code != http.StatusUnauthorized || strings.Contains(recorder.Body.String(), bearer) {
				t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestRemoteCaptchaHTTPCancelRequiresBoundViewerAndStrictRequest(t *testing.T) {
	s, bearer, _ := newRemoteCaptchaHTTPFixture(t)
	redeemBody := fmt.Sprintf(`{"token":%q}`, bearer)
	redeemed := serveRemoteCaptchaHTTP(s, remoteCaptchaHTTPRequest(http.MethodPost, "/api/auth/captcha/remote/redeem", remoteCaptchaTestOrigin, redeemBody))
	if redeemed.Code != http.StatusNoContent || len(redeemed.Result().Cookies()) != 1 {
		t.Fatalf("redeem status=%d cookies=%d", redeemed.Code, len(redeemed.Result().Cookies()))
	}
	cookie := redeemed.Result().Cookies()[0]

	for _, test := range []struct {
		name       string
		origin     string
		body       string
		withCookie bool
		wantStatus int
	}{
		{name: "missing origin", body: `{}`, withCookie: true, wantStatus: http.StatusForbidden},
		{name: "missing cookie", origin: remoteCaptchaTestOrigin, body: `{}`, wantStatus: http.StatusUnauthorized},
		{name: "null json", origin: remoteCaptchaTestOrigin, body: `null`, withCookie: true, wantStatus: http.StatusBadRequest},
		{name: "unknown json", origin: remoteCaptchaTestOrigin, body: `{"owner":"discord-owner"}`, withCookie: true, wantStatus: http.StatusBadRequest},
		{name: "oversized body", origin: remoteCaptchaTestOrigin, body: `{"padding":"` + strings.Repeat("a", captchaSubmitBodyLimit) + `"}`, withCookie: true, wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := remoteCaptchaHTTPRequest(http.MethodPost, "/api/auth/captcha/remote/cancel", test.origin, test.body)
			if test.withCookie {
				req.AddCookie(cookie)
			}
			recorder := serveRemoteCaptchaHTTP(s, req)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%q", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}

	req := remoteCaptchaHTTPRequest(http.MethodPost, "/api/auth/captcha/remote/cancel", remoteCaptchaTestOrigin, `{}`)
	req.AddCookie(cookie)
	recorder := serveRemoteCaptchaHTTP(s, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if _, err := s.lookupRemoteCaptchaViewer(cookie.Value); err == nil {
		t.Fatal("viewer session survived explicit cancel")
	}
}
