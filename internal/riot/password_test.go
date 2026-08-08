package riot_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    req,
	}
}

type readErrorBody struct {
	data []byte
	err  error
	done bool
}

func (b *readErrorBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, b.err
	}
	b.done = true
	return copy(p, b.data), b.err
}

func (*readErrorBody) Close() error { return nil }

const captchaBrowserUserAgent = "captcha-browser/1"

func captchaCookieValues(cookies []*http.Cookie) map[string]string {
	values := make(map[string]string, len(cookies))
	for _, cookie := range cookies {
		if cookie != nil && cookie.Value != "" && cookie.MaxAge >= 0 {
			values[cookie.Name] = cookie.Value
		}
	}
	return values
}

func captchaBrowserSession(ch riot.CaptchaChallenge) riot.CaptchaBrowserSession {
	return riot.CaptchaBrowserSession{
		UserAgent: captchaBrowserUserAgent,
		Cookies:   captchaCookieValues(ch.BrowserCookies),
	}
}

func mountCaptchaWebEntry(mux *http.ServeMux) {
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login-page", http.StatusFound)
	})
	mux.HandleFunc("/login-page", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "login")
	})
}

func TestPasswordBrowserAuthorizeURLAndAdoptMFA(t *testing.T) {
	var mfaCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", r.Method)
		}
		if got := r.UserAgent(); got != captchaBrowserUserAgent {
			t.Fatalf("User-Agent = %q, want %q", got, captchaBrowserUserAgent)
		}
		if got, want := r.Header.Get("Origin"), "http://"+r.Host; got != want {
			t.Fatalf("Origin = %q, want %q", got, want)
		}
		if got := r.Header.Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("Cache-Control = %q, want no-cache", got)
		}
		if cookie, err := r.Cookie("authenticator.sid"); err != nil || cookie.Value != "browser-session" {
			t.Fatalf("authenticator.sid = %v, %v", cookie, err)
		}
		if _, err := r.Cookie("tracking"); err == nil {
			t.Fatal("non-allowlisted browser cookie reached MFA request")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		multifactor, _ := body["multifactor"].(map[string]any)
		if body["type"] != "multifactor" || multifactor["otp"] != "123456" {
			t.Fatalf("MFA body = %#v", body)
		}
		mfaCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":    "success",
			"success": map[string]any{"login_token": "browser-login-token"},
		})
	})
	mux.HandleFunc("/api/v1/login-token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": map[string]any{
				"parameters": map[string]any{
					"uri": "http://localhost/redirect#access_token=browser-at&id_token=browser-id",
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{
		HTTPClient:          srv.Client(),
		AuthBaseURL:         srv.URL,
		AuthenticateBaseURL: srv.URL,
	}
	authorizeURL, err := c.BrowserAuthorizeURL("discord-state")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/authorize" || parsed.Query().Get("nonce") != "discord-state" ||
		parsed.Query().Get("client_id") != "riot-client" ||
		parsed.Query().Get("redirect_uri") != "http://localhost/redirect" ||
		parsed.Query().Get("response_type") != "token id_token" ||
		parsed.Query().Get("scope") != "openid link ban lol_region account" {
		t.Fatalf("authorize URL = %s", authorizeURL)
	}

	_, challenge, err := c.AdoptBrowserLogin(context.Background(), []byte(`{
		"type":"multifactor",
		"multifactor":{"email":"b***@example.com","method":"email","methods":["riotmobile","thirdparty","email"]}
	}`), []*http.Cookie{
		{Name: "authenticator.sid", Value: "browser-session", Path: "/", Secure: true, HttpOnly: true},
		{Name: "tracking", Value: "must-not-forward", Path: "/", Secure: true},
	}, captchaBrowserUserAgent)
	if err != nil || challenge == nil {
		t.Fatalf("adopt browser MFA: challenge=%#v err=%v", challenge, err)
	}
	if challenge.Email != "b***@example.com" || challenge.Method != "email" || len(challenge.Methods) != 3 {
		t.Fatalf("challenge = %#v", challenge)
	}

	tokens, err := c.SubmitMFA(context.Background(), challenge, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "browser-at" || tokens.IDToken != "browser-id" || mfaCalls.Load() != 1 {
		t.Fatalf("tokens=%+v mfaCalls=%d", tokens, mfaCalls.Load())
	}
}

func TestPasswordBeginAndCompleteCaptcha_UsesBrowserSession(t *testing.T) {
	var puts atomic.Int32
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != captchaBrowserUserAgent {
			t.Fatalf("User-Agent = %q, want %q", got, captchaBrowserUserAgent)
		}
		switch r.Method {
		case http.MethodGet:
			http.SetCookie(w, &http.Cookie{Name: "authenticator.sid", Value: "s1", Path: "/", Secure: true, HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "tdid", Value: "d1", Path: "/", Secure: true, HttpOnly: true})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"captcha": map[string]any{
					"hcaptcha": map[string]string{"key": "site-key", "data": "rq-data"},
				},
			})
		case http.MethodPut:
			puts.Add(1)
			for name, want := range map[string]string{"authenticator.sid": "s1", "tdid": "d1"} {
				cookie, err := r.Cookie(name)
				if err != nil || cookie.Value != want {
					t.Fatalf("PUT cookie %s = %v, %v; want %q", name, cookie, err, want)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":    "success",
				"success": map[string]any{"login_token": "lt-browser"},
			})
		}
	})
	mux.HandleFunc("/api/v1/login-token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": map[string]any{
				"parameters": map[string]any{
					"uri": "http://localhost/redirect#access_token=at-browser&id_token=id-browser",
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{
		HTTPClient:          srv.Client(),
		AuthBaseURL:         srv.URL,
		AuthenticateBaseURL: srv.URL,
	}
	browser := riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent}
	ch, err := c.BeginCaptcha(context.Background(), "user", "secret", browser)
	if err != nil {
		t.Fatal(err)
	}
	cookieValues := captchaCookieValues(ch.BrowserCookies)
	if cookieValues["authenticator.sid"] != "s1" || cookieValues["tdid"] != "d1" {
		t.Fatalf("browser cookies = %#v", cookieValues)
	}
	browser.Cookies = cookieValues
	tokens, mfa, err := c.CompleteCaptcha(context.Background(), ch.SessionID, "captcha-token", browser)
	if err != nil || mfa != nil {
		t.Fatalf("complete: tokens=%+v mfa=%v err=%v", tokens, mfa, err)
	}
	if tokens.AccessToken != "at-browser" || puts.Load() != 1 {
		t.Fatalf("tokens=%+v puts=%d", tokens, puts.Load())
	}
}

func TestPasswordBeginCaptchaSeedsRiotSessionFromWebAuthorization(t *testing.T) {
	var authorizeCalls atomic.Int32
	var loginSawAuthorizationCookie atomic.Bool
	var loginSawOutOfScopeCookie atomic.Bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Host == "valorant-api.com":
			return jsonResponse(r, `{"data":{"riotClientVersion":"release-test","riotClientBuild":"test"}}`), nil
		case r.URL.Host == "auth.riotgames.com" && r.URL.Path == "/authorize":
			authorizeCalls.Add(1)
			header := make(http.Header)
			header.Set("Location", "https://authenticate.riotgames.com/login-page")
			header.Add("Set-Cookie", "__cf_bm=cloudflare-session; Domain=riotgames.com; Path=/; Secure; HttpOnly; SameSite=None")
			header.Add("Set-Cookie", "tracking=not-allowlisted; Domain=riotgames.com; Path=/; Secure")
			header.Add("Set-Cookie", "tdid=host-only; Path=/; Secure; HttpOnly")
			header.Add("Set-Cookie", "authenticator.sid=wrong-path; Domain=riotgames.com; Path=/authorize-only; Secure; HttpOnly")
			header.Add("Set-Cookie", "__cflb=not-secure; Domain=riotgames.com; Path=/; HttpOnly")
			return &http.Response{
				StatusCode: http.StatusFound,
				Status:     "302 Found",
				Header:     header,
				Body:       io.NopCloser(strings.NewReader("redirect")),
				Request:    r,
			}, nil
		case r.URL.Host == "authenticate.riotgames.com" && r.URL.Path == "/login-page":
			return jsonResponse(r, "login"), nil
		case r.URL.Host == "authenticate.riotgames.com" && r.URL.Path == "/api/v1/login" && r.Method == http.MethodGet:
			if cookie, err := r.Cookie("__cf_bm"); err == nil && cookie.Value == "cloudflare-session" {
				loginSawAuthorizationCookie.Store(true)
			}
			for _, name := range []string{"tracking", "tdid", "authenticator.sid", "__cflb"} {
				if _, err := r.Cookie(name); err == nil {
					loginSawOutOfScopeCookie.Store(true)
				}
			}
			return jsonResponse(r, `{"captcha":{"hcaptcha":{"key":"site-key","data":"rq-data"}}}`), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		}
	})}

	c := &riot.PasswordClient{HTTPClient: client}
	challenge, err := c.BeginCaptcha(context.Background(), "user", "secret", riot.CaptchaBrowserSession{
		UserAgent: captchaBrowserUserAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := authorizeCalls.Load(); got != 1 {
		t.Fatalf("authorization calls = %d, want 1 before captcha login", got)
	}
	if !loginSawAuthorizationCookie.Load() {
		t.Fatal("captcha login did not receive the Riot web authorization session cookie")
	}
	if loginSawOutOfScopeCookie.Load() {
		t.Fatal("captcha login received an out-of-scope discovery cookie")
	}
	var browserCookie *http.Cookie
	for _, cookie := range challenge.BrowserCookies {
		if cookie != nil && cookie.Name == "__cf_bm" && cookie.Value != "" {
			browserCookie = cookie
		}
		if cookie != nil && cookie.Value != "" && cookie.Name != "__cf_bm" {
			t.Fatalf("browser received out-of-scope authorization cookie: %#v", cookie)
		}
	}
	if browserCookie == nil {
		t.Fatal("browser did not receive the authorization __cf_bm cookie")
	}
	if browserCookie.Domain != "riotgames.com" || browserCookie.Path != "/" ||
		!browserCookie.Secure || !browserCookie.HttpOnly || browserCookie.SameSite != http.SameSiteNoneMode {
		t.Fatalf("browser authorization cookie attributes changed: %#v", browserCookie)
	}
}

func TestPasswordCompleteCaptchaRejectsDifferentBrowserSession(t *testing.T) {
	var puts atomic.Int32
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
		}
		http.SetCookie(w, &http.Cookie{Name: "authenticator.sid", Value: "s1"})
		http.SetCookie(w, &http.Cookie{Name: "tdid", Value: "d1"})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"captcha": map[string]any{
				"hcaptcha": map[string]string{"key": "site-key", "data": "rq-data"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL, AuthenticateBaseURL: srv.URL}
	ch, err := c.BeginCaptcha(context.Background(), "user", "secret", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.CompleteCaptcha(context.Background(), ch.SessionID, "captcha-token", riot.CaptchaBrowserSession{
		UserAgent: captchaBrowserUserAgent,
		Cookies:   map[string]string{"authenticator.sid": "s1"},
	})
	if !errors.Is(err, riot.ErrCaptchaSession) {
		t.Fatalf("error = %v, want ErrCaptchaSession", err)
	}
	if got := puts.Load(); got != 0 {
		t.Fatalf("Riot PUT calls = %d, want 0 for mismatched browser cookies", got)
	}

	_, _, err = c.CompleteCaptcha(context.Background(), ch.SessionID, "captcha-token", riot.CaptchaBrowserSession{
		UserAgent: captchaBrowserUserAgent,
		Cookies: map[string]string{
			"authenticator.sid": "s1",
			"tdid":              "d1",
			"__cf_bm":           "stale-extra",
		},
	})
	if !errors.Is(err, riot.ErrCaptchaSession) {
		t.Fatalf("extra-cookie error = %v, want ErrCaptchaSession", err)
	}
	if got := puts.Load(); got != 0 {
		t.Fatalf("Riot PUT calls = %d, want 0 for extra browser cookies", got)
	}
}

func TestPasswordBeginCaptchaClearsStaleBrowserCookies(t *testing.T) {
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("tdid"); err == nil {
			t.Fatal("stale browser cookie must not be imported into a replacement Riot session")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"captcha": map[string]any{
				"hcaptcha": map[string]string{"key": "site-key", "data": "rq-data"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL, AuthenticateBaseURL: srv.URL}
	ch, err := c.BeginCaptcha(context.Background(), "user", "secret", riot.CaptchaBrowserSession{
		UserAgent: captchaBrowserUserAgent,
		Cookies:   map[string]string{"tdid": "stale-browser-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hostClear, domainClear := false, false
	for _, cookie := range ch.BrowserCookies {
		if cookie.Name != "tdid" || cookie.Value != "" || cookie.MaxAge >= 0 || cookie.Path != "/" {
			continue
		}
		if cookie.Domain == "" {
			hostClear = true
		}
		if cookie.Domain == "riotgames.com" {
			domainClear = true
		}
	}
	if !hostClear || !domainClear {
		t.Fatalf("challenge did not clear both stale tdid variants: %#v", ch.BrowserCookies)
	}
}

func TestPasswordBeginCaptchaRefreshesMetadataOnceConcurrently(t *testing.T) {
	var metaCalls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "valorant-api.com" {
			metaCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			return jsonResponse(req, `{"data":{"riotClientVersion":"release-10.0.1234","riotClientBuild":"1234"}}`), nil
		}
		return jsonResponse(req, `{"captcha":{"hcaptcha":{"key":"site-key","data":"rq-data"}}}`), nil
	})}
	c := &riot.PasswordClient{
		HTTPClient:          client,
		AuthBaseURL:         "https://authenticate.test",
		AuthenticateBaseURL: "https://authenticate.test",
	}

	const callers = 12
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			if _, err := c.BeginCaptcha(context.Background(), "user", "pass", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent}); err != nil {
				t.Errorf("BeginCaptcha(%d): %v", i, err)
			}
		}()
	}
	wg.Wait()
	if got := metaCalls.Load(); got != 1 {
		t.Fatalf("metadata requests = %d, want 1", got)
	}
}

func TestPasswordBeginCaptchaMetadataFailureUsesFallbackOnce(t *testing.T) {
	var metaCalls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "valorant-api.com" {
			metaCalls.Add(1)
			return nil, errors.New("version service unavailable")
		}
		return jsonResponse(req, `{"captcha":{"hcaptcha":{"key":"site-key","data":"rq-data"}}}`), nil
	})}
	c := &riot.PasswordClient{
		HTTPClient:          client,
		AuthBaseURL:         "https://authenticate.test",
		AuthenticateBaseURL: "https://authenticate.test",
	}

	const callers = 12
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			if _, err := c.BeginCaptcha(context.Background(), "user", "pass", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent}); err != nil {
				t.Errorf("BeginCaptcha(%d): %v", i, err)
			}
		}()
	}
	wg.Wait()
	if got := metaCalls.Load(); got != 1 {
		t.Fatalf("failed metadata requests = %d, want one shared attempt then fallback", got)
	}
}

func TestPasswordBeginAndCompleteCaptcha_Success(t *testing.T) {
	var beginBaggage string
	var webSessionOrder atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("web authorization method = %s, want GET", r.Method)
		}
		if r.URL.Query().Get("client_id") != "riot-client" || r.URL.Query().Get("redirect_uri") != "http://localhost/redirect" {
			t.Fatalf("web authorization query = %q", r.URL.RawQuery)
		}
		if got := webSessionOrder.Add(1); got != 1 {
			t.Fatalf("web authorization order = %d, want 1", got)
		}
		http.Redirect(w, r, "/login-page", http.StatusFound)
	})
	mux.HandleFunc("/login-page", func(w http.ResponseWriter, _ *http.Request) {
		if got := webSessionOrder.Add(1); got != 2 {
			t.Fatalf("login page order = %d, want 2", got)
		}
		http.SetCookie(w, &http.Cookie{Name: "authenticator.sid", Value: "s1", Path: "/", Secure: true, HttpOnly: true})
		_, _ = io.WriteString(w, "login")
	})
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != captchaBrowserUserAgent {
			t.Fatalf("authenticate User-Agent = %q, want %q", got, captchaBrowserUserAgent)
		}
		switch r.Method {
		case http.MethodGet:
			if got := webSessionOrder.Add(1); got != 3 {
				t.Fatalf("captcha challenge order = %d, want 3", got)
			}
			beginBaggage = r.Header.Get("baggage")
			if beginBaggage != "" {
				t.Fatalf("web captcha begin baggage = %q, want none", beginBaggage)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "auth",
				"captcha": map[string]any{
					"type": "hcaptcha",
					"hcaptcha": map[string]string{
						"key":  "site-key",
						"data": "rq-data",
					},
				},
			})
		case http.MethodPut:
			if got := r.Header.Get("baggage"); got != beginBaggage {
				t.Fatalf("captcha completion baggage = %q, want %q", got, beginBaggage)
			}
			if cookie, err := r.Cookie("authenticator.sid"); err != nil || cookie.Value != "s1" {
				t.Fatalf("captcha completion missing begin-session cookie: cookie=%v err=%v", cookie, err)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["language"] != "ko_KR" || body["remember"] != false {
				t.Fatalf("top-level language/remember = %#v/%#v, body=%#v", body["language"], body["remember"], body)
			}
			identity, ok := body["riot_identity"].(map[string]any)
			if !ok || identity["username"] != "user1" || identity["captcha"] != "hcaptcha tok" {
				t.Fatalf("riot_identity = %#v", body["riot_identity"])
			}
			if _, exists := identity["language"]; exists {
				t.Fatalf("language must not be nested under riot_identity: %#v", identity)
			}
			if _, exists := identity["remember"]; exists {
				t.Fatalf("remember must not be nested under riot_identity: %#v", identity)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "success",
				"success": map[string]any{
					"login_token": "lt-1",
				},
			})
		default:
			t.Fatalf("captcha login method = %s, want web GET then PUT", r.Method)
		}
	})
	mux.HandleFunc("/api/v1/login-token", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "ssid", Value: "session"})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "response",
			"response": map[string]any{
				"parameters": map[string]any{
					"uri": "http://localhost/redirect#access_token=at-1&id_token=id-1",
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{
		HTTPClient:          srv.Client(),
		AuthBaseURL:         srv.URL,
		AuthenticateBaseURL: srv.URL,
	}
	ch, err := c.BeginCaptcha(context.Background(), "user1", "secret", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	if ch.SiteKey != "site-key" || ch.RQData != "rq-data" || ch.SessionID == "" {
		t.Fatalf("%+v", ch)
	}
	tok, mfa, err := c.CompleteCaptcha(context.Background(), ch.SessionID, "tok", captchaBrowserSession(ch))
	if err != nil {
		t.Fatal(err)
	}
	if mfa != nil {
		t.Fatal("unexpected mfa")
	}
	if tok.AccessToken != "at-1" {
		t.Fatalf("%+v", tok)
	}
}

func TestPasswordCancelCaptchaDeletesSession(t *testing.T) {
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"captcha": map[string]any{
				"hcaptcha": map[string]string{"key": "k", "data": "d"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL, AuthenticateBaseURL: srv.URL}
	ch, err := c.BeginCaptcha(context.Background(), "user", "secret", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	c.CancelCaptcha(ch.SessionID)
	_, _, err = c.CompleteCaptcha(context.Background(), ch.SessionID, "token", captchaBrowserSession(ch))
	if !errors.Is(err, riot.ErrCaptchaSession) {
		t.Fatalf("completion after cancel = %v, want ErrCaptchaSession", err)
	}
}

func TestPasswordCompleteCaptcha_MFA(t *testing.T) {
	var sessionBaggage string
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != captchaBrowserUserAgent {
			t.Fatalf("authenticate User-Agent = %q, want %q", got, captchaBrowserUserAgent)
		}
		switch r.Method {
		case http.MethodGet:
			sessionBaggage = r.Header.Get("baggage")
			if sessionBaggage != "" {
				t.Fatalf("web captcha begin baggage = %q, want none", sessionBaggage)
			}
			http.SetCookie(w, &http.Cookie{Name: "authenticator.sid", Value: "initial-session", Path: "/", Secure: true, HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "tdid", Value: "initial-device", Domain: "riotgames.com", Path: "/", Secure: true, HttpOnly: true})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"captcha": map[string]any{
					"hcaptcha": map[string]string{"key": "k", "data": "d"},
				},
			})
		case http.MethodPut:
			if got := r.Header.Get("baggage"); got != sessionBaggage {
				t.Fatalf("captcha/MFA baggage = %q, want %q", got, sessionBaggage)
			}
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"type":"multifactor"`) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"type": "success",
					"success": map[string]any{
						"login_token": "lt-mfa",
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "multifactor",
				"multifactor": map[string]any{
					"email":   "a***@ex.com",
					"method":  "email",
					"methods": []string{"email"},
				},
			})
		}
	})
	mux.HandleFunc("/api/v1/login-token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": map[string]any{
				"parameters": map[string]any{
					"uri": "http://localhost/redirect#access_token=at-mfa&id_token=id-mfa",
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{
		HTTPClient:          srv.Client(),
		AuthBaseURL:         srv.URL,
		AuthenticateBaseURL: srv.URL,
	}
	ch, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	_, mfa, err := c.CompleteCaptcha(context.Background(), ch.SessionID, "tok", captchaBrowserSession(ch))
	if err != nil {
		t.Fatal(err)
	}
	if mfa == nil || mfa.Email != "a***@ex.com" {
		t.Fatalf("mfa %+v", mfa)
	}
	tok, err := c.SubmitMFA(context.Background(), mfa, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at-mfa" {
		t.Fatalf("%+v", tok)
	}
}

func TestPasswordCompleteCaptchaRejectsAmbiguousNonOKContinuationBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "mfa shaped", body: `{"type":"multifactor","multifactor":{"method":"email"}}`},
		{name: "success shaped", body: `{"type":"success","success":{"login_token":"must-not-exchange"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tokenExchanges atomic.Int32
			mux := http.NewServeMux()
			mountCaptchaWebEntry(mux)
			mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					_, _ = io.WriteString(w, `{"captcha":{"hcaptcha":{"key":"k","data":"d"}}}`)
				case http.MethodPut:
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, tt.body)
				}
			})
			mux.HandleFunc("/api/v1/login-token", func(w http.ResponseWriter, _ *http.Request) {
				tokenExchanges.Add(1)
				w.WriteHeader(http.StatusNoContent)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL, AuthenticateBaseURL: srv.URL}
			challenge, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
			if err != nil {
				t.Fatal(err)
			}
			tokens, mfa, err := c.CompleteCaptcha(context.Background(), challenge.SessionID, "captcha-token", captchaBrowserSession(challenge))
			if err == nil || mfa != nil || tokens.AccessToken != "" {
				t.Fatalf("ambiguous non-OK completion: tokens=%+v mfa=%v err=%v", tokens, mfa, err)
			}
			if got := tokenExchanges.Load(); got != 0 {
				t.Fatalf("token exchanges=%d, want 0", got)
			}
		})
	}
}

func TestPasswordSubmitMFADoesNotRetryAfterLoginTokenExchangeFailure(t *testing.T) {
	var mfaPUTs atomic.Int32
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"captcha": map[string]any{
					"hcaptcha": map[string]string{"key": "k", "data": "d"},
				},
			})
		case http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode PUT body: %v", err)
			}
			if body["type"] == "auth" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"type":        "multifactor",
					"multifactor": map[string]any{"method": "email"},
				})
				return
			}
			mfaPUTs.Add(1)
			multifactor, ok := body["multifactor"].(map[string]any)
			if !ok || multifactor["otp"] != "123456" || body["code"] != nil {
				t.Fatalf("canonical MFA body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":    "success",
				"success": map[string]any{"login_token": "consumed-login-token"},
			})
		}
	})
	mux.HandleFunc("/api/v1/login-token", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL, AuthenticateBaseURL: srv.URL}
	challenge, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	_, mfa, err := c.CompleteCaptcha(context.Background(), challenge.SessionID, "captcha-token", captchaBrowserSession(challenge))
	if err != nil || mfa == nil {
		t.Fatalf("captcha completion: mfa=%v err=%v", mfa, err)
	}
	if _, err := c.SubmitMFA(context.Background(), mfa, "123456"); err == nil {
		t.Fatal("MFA succeeded after login-token exchange failure")
	}
	if got := mfaPUTs.Load(); got != 1 {
		t.Fatalf("MFA PUTs after a consuming success = %d, want 1", got)
	}
}

func TestPasswordSubmitMFADoesNotRetryAfterTransportFailure(t *testing.T) {
	var mfaPUTs atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/authorize" {
			return jsonResponse(r, `login`), nil
		}
		if r.URL.Path != "/api/v1/login" {
			return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			return jsonResponse(r, `{"captcha":{"hcaptcha":{"key":"k","data":"d"}}}`), nil
		case http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, err
			}
			if body["type"] == "auth" {
				return jsonResponse(r, `{"type":"multifactor","multifactor":{"method":"email"}}`), nil
			}
			mfaPUTs.Add(1)
			return nil, errors.New("network connection reset")
		default:
			return nil, fmt.Errorf("unexpected login method %s", r.Method)
		}
	})}
	c := &riot.PasswordClient{HTTPClient: client, AuthBaseURL: "https://riot.test", AuthenticateBaseURL: "https://riot.test"}
	challenge, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	_, mfa, err := c.CompleteCaptcha(context.Background(), challenge.SessionID, "captcha-token", captchaBrowserSession(challenge))
	if err != nil || mfa == nil {
		t.Fatalf("captcha completion: mfa=%v err=%v", mfa, err)
	}
	if _, err := c.SubmitMFA(context.Background(), mfa, "123456"); err == nil {
		t.Fatal("MFA succeeded after transport failure")
	}
	if got := mfaPUTs.Load(); got != 1 {
		t.Fatalf("MFA PUTs after transport ambiguity = %d, want 1", got)
	}
}

func TestPasswordSubmitMFARejectsAmbiguousNonOKBodiesBeforeParsing(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "mfa shaped", body: `{"type":"multifactor","multifactor":{"method":"email"}}`},
		{name: "success shaped", body: `{"type":"success","success":{"login_token":"must-not-exchange"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mfaPUTs atomic.Int32
			var tokenExchanges atomic.Int32
			mux := http.NewServeMux()
			mountCaptchaWebEntry(mux)
			mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					_ = json.NewEncoder(w).Encode(map[string]any{
						"captcha": map[string]any{
							"hcaptcha": map[string]string{"key": "k", "data": "d"},
						},
					})
				case http.MethodPut:
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatalf("decode PUT body: %v", err)
					}
					if body["type"] == "auth" {
						_ = json.NewEncoder(w).Encode(map[string]any{
							"type":        "multifactor",
							"multifactor": map[string]any{"method": "email"},
						})
						return
					}
					mfaPUTs.Add(1)
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, tt.body)
				}
			})
			mux.HandleFunc("/api/v1/login-token", func(w http.ResponseWriter, _ *http.Request) {
				tokenExchanges.Add(1)
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"response": map[string]any{"parameters": map[string]string{
						"uri": "http://localhost/redirect#access_token=at&id_token=id",
					}},
				})
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL, AuthenticateBaseURL: srv.URL}
			challenge, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
			if err != nil {
				t.Fatal(err)
			}
			_, mfa, err := c.CompleteCaptcha(context.Background(), challenge.SessionID, "captcha-token", captchaBrowserSession(challenge))
			if err != nil || mfa == nil {
				t.Fatalf("captcha completion: mfa=%v err=%v", mfa, err)
			}
			_, err = c.SubmitMFA(context.Background(), mfa, "123456")
			if err == nil {
				t.Fatal("ambiguous non-OK MFA response was accepted")
			}
			if errors.Is(err, riot.ErrPasswordInvalidCode) {
				t.Fatalf("ambiguous non-OK MFA response remained retryable: %v", err)
			}
			if got := mfaPUTs.Load(); got != 1 {
				t.Fatalf("MFA PUTs=%d, want exactly 1", got)
			}
			if got := tokenExchanges.Load(); got != 0 {
				t.Fatalf("token exchanges=%d, want 0", got)
			}
		})
	}
}

func TestPasswordSubmitMFADoesNotFallbackAfterSchemaBodyReadError(t *testing.T) {
	var loginPUTs atomic.Int32
	sentinel := errors.New("truncated response")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/authorize" {
			return jsonResponse(r, `login`), nil
		}
		if r.URL.Path != "/api/v1/login" {
			return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			return jsonResponse(r, `{"captcha":{"hcaptcha":{"key":"k","data":"d"}}}`), nil
		case http.MethodPut:
			put := loginPUTs.Add(1)
			if put == 1 {
				return jsonResponse(r, `{"type":"multifactor","multifactor":{"method":"email"}}`), nil
			}
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Status:     "400 Bad Request",
				Header:     make(http.Header),
				Body: &readErrorBody{
					data: []byte(`{"type":"error","error":"invalid_request","error_description":"multifactor request schema rejected before otp processing"}`),
					err:  sentinel,
				},
				Request: r,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected login method %s", r.Method)
		}
	})}
	c := &riot.PasswordClient{HTTPClient: client, AuthBaseURL: "https://riot.test", AuthenticateBaseURL: "https://riot.test"}
	challenge, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	_, mfa, err := c.CompleteCaptcha(context.Background(), challenge.SessionID, "captcha-token", captchaBrowserSession(challenge))
	if err != nil || mfa == nil {
		t.Fatalf("captcha completion: mfa=%v err=%v", mfa, err)
	}
	_, err = c.SubmitMFA(context.Background(), mfa, "123456")
	if err == nil || errors.Is(err, riot.ErrPasswordInvalidCode) {
		t.Fatalf("schema response read ambiguity remained retryable: %v", err)
	}
	if got := loginPUTs.Load(); got != 2 {
		t.Fatalf("login PUTs=%d, want captcha plus exactly one MFA PUT", got)
	}
}

func TestPasswordSubmitMFADoesNotFollowRedirectOrReplayOTP(t *testing.T) {
	var redirectedPUTs atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			redirectedPUTs.Add(1)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(redirectTarget.Close)

	var loginPUTs atomic.Int32
	var tokenExchanges atomic.Int32
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"captcha":{"hcaptcha":{"key":"k","data":"d"}}}`)
		case http.MethodPut:
			put := loginPUTs.Add(1)
			if put == 1 {
				_, _ = io.WriteString(w, `{"type":"multifactor","multifactor":{"method":"email"}}`)
				return
			}
			w.Header().Set("Location", redirectTarget.URL+"/otp-replay")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
	})
	mux.HandleFunc("/api/v1/login-token", func(w http.ResponseWriter, _ *http.Request) {
		tokenExchanges.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL, AuthenticateBaseURL: srv.URL}
	challenge, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	_, mfa, err := c.CompleteCaptcha(context.Background(), challenge.SessionID, "captcha-token", captchaBrowserSession(challenge))
	if err != nil || mfa == nil {
		t.Fatalf("captcha completion: mfa=%v err=%v", mfa, err)
	}
	_, err = c.SubmitMFA(context.Background(), mfa, "123456")
	if err == nil || errors.Is(err, riot.ErrPasswordInvalidCode) {
		t.Fatalf("redirected MFA remained retryable: %v", err)
	}
	if got := loginPUTs.Load(); got != 2 {
		t.Fatalf("login PUTs=%d, want captcha plus exactly one MFA PUT", got)
	}
	if got := redirectedPUTs.Load(); got != 0 {
		t.Fatalf("redirect target PUTs=%d, want 0", got)
	}
	if got := tokenExchanges.Load(); got != 0 {
		t.Fatalf("token exchanges=%d, want 0", got)
	}
}

func TestPasswordSubmitMFAFallsBackAfterExplicitSchemaRejection(t *testing.T) {
	var canonicalPUTs atomic.Int32
	var alternatePUTs atomic.Int32
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"captcha": map[string]any{
					"hcaptcha": map[string]string{"key": "k", "data": "d"},
				},
			})
		case http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode PUT body: %v", err)
			}
			if body["type"] == "auth" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"type":        "multifactor",
					"multifactor": map[string]any{"method": "email"},
				})
				return
			}
			if multifactor, ok := body["multifactor"].(map[string]any); ok && multifactor["otp"] == "123456" {
				canonicalPUTs.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"type":              "error",
					"error":             "invalid_request",
					"error_description": "multifactor request schema rejected before otp processing",
				})
				return
			}
			if body["type"] != "multifactor" || body["code"] != "123456" || body["rememberDevice"] != true {
				t.Fatalf("alternate MFA body = %#v", body)
			}
			alternatePUTs.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":    "success",
				"success": map[string]any{"login_token": "fallback-login-token"},
			})
		}
	})
	mux.HandleFunc("/api/v1/login-token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": map[string]any{
				"parameters": map[string]string{"uri": "http://localhost/redirect#access_token=at&id_token=id"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL, AuthenticateBaseURL: srv.URL}
	challenge, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	_, mfa, err := c.CompleteCaptcha(context.Background(), challenge.SessionID, "captcha-token", captchaBrowserSession(challenge))
	if err != nil || mfa == nil {
		t.Fatalf("captcha completion: mfa=%v err=%v", mfa, err)
	}
	tokens, err := c.SubmitMFA(context.Background(), mfa, "123456")
	if err != nil {
		t.Fatalf("MFA fallback: %v", err)
	}
	if tokens.AccessToken != "at" {
		t.Fatalf("fallback tokens = %+v", tokens)
	}
	if got := canonicalPUTs.Load(); got != 1 {
		t.Fatalf("canonical MFA PUTs = %d, want 1", got)
	}
	if got := alternatePUTs.Load(); got != 1 {
		t.Fatalf("alternate MFA PUTs = %d, want 1", got)
	}
}

func TestPasswordCompleteCaptcha_AuthFailure(t *testing.T) {
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"captcha": map[string]any{
					"hcaptcha": map[string]string{"key": "k", "data": "d"},
				},
			})
		case http.MethodPut:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":  "auth",
				"error": "auth_failure",
			})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{
		HTTPClient:          srv.Client(),
		AuthBaseURL:         srv.URL,
		AuthenticateBaseURL: srv.URL,
	}
	ch, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.CompleteCaptcha(context.Background(), ch.SessionID, "tok", captchaBrowserSession(ch))
	if !errors.Is(err, riot.ErrPasswordInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestPasswordCompleteCaptcha_AuthFailureWithCaptchaIsRetry(t *testing.T) {
	// Riot often returns auth_failure AND a new captcha together for bad tokens.
	// Treat that as captcha retry (not a hard password error).
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"captcha": map[string]any{
					"hcaptcha": map[string]string{"key": "k", "data": "d"},
				},
			})
		case http.MethodPut:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":  "auth",
				"error": "auth_failure",
				"captcha": map[string]any{
					"hcaptcha": map[string]string{"key": "k2", "data": "d2"},
				},
			})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{
		HTTPClient:          srv.Client(),
		AuthBaseURL:         srv.URL,
		AuthenticateBaseURL: srv.URL,
	}
	ch, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.CompleteCaptcha(context.Background(), ch.SessionID, "tok", captchaBrowserSession(ch))
	var retry *riot.CaptchaRetryError
	if !errors.As(err, &retry) {
		t.Fatalf("err=%v want CaptchaRetryError", err)
	}
	if retry.SiteKey != "k2" || retry.RQData != "d2" {
		t.Fatalf("retry=%+v", retry)
	}
}

func TestPasswordCompleteCaptcha_RetryKeepsSession(t *testing.T) {
	puts := 0
	var sessionBaggage string
	mux := http.NewServeMux()
	mountCaptchaWebEntry(mux)
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sessionBaggage = r.Header.Get("baggage")
			if sessionBaggage != "" {
				t.Fatalf("web captcha begin baggage = %q, want none", sessionBaggage)
			}
			http.SetCookie(w, &http.Cookie{Name: "authenticator.sid", Value: "initial-session", Path: "/", Secure: true, HttpOnly: true})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "auth",
				"captcha": map[string]any{
					"hcaptcha": map[string]string{"key": "k1", "data": "d1"},
				},
			})
		case http.MethodPut:
			if got := r.Header.Get("baggage"); got != sessionBaggage {
				t.Fatalf("captcha retry baggage = %q, want %q", got, sessionBaggage)
			}
			puts++
			if puts == 1 {
				http.SetCookie(w, &http.Cookie{Name: "authenticator.sid", Value: "retry-session", Path: "/", Secure: true, HttpOnly: true})
				http.SetCookie(w, &http.Cookie{Name: "tdid", Value: "", Domain: "riotgames.com", Path: "/", Secure: true, HttpOnly: true, MaxAge: -1})
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"type":  "auth",
					"error": "captcha_not_allowed",
					"captcha": map[string]any{
						"hcaptcha": map[string]string{"key": "k2", "data": "d2"},
					},
				})
				return
			}
			if cookie, err := r.Cookie("authenticator.sid"); err != nil || cookie.Value != "retry-session" {
				t.Fatalf("retry completion missing updated cookie: cookie=%v err=%v", cookie, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "success",
				"success": map[string]any{
					"login_token": "lt-retry",
				},
			})
		}
	})
	mux.HandleFunc("/api/v1/login-token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": map[string]any{
				"parameters": map[string]any{
					"uri": "http://localhost/redirect#access_token=at-r&id_token=id-r",
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{
		HTTPClient:          srv.Client(),
		AuthBaseURL:         srv.URL,
		AuthenticateBaseURL: srv.URL,
	}
	ch, err := c.BeginCaptcha(context.Background(), "u", "p", riot.CaptchaBrowserSession{UserAgent: captchaBrowserUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	browser := captchaBrowserSession(ch)
	_, _, err = c.CompleteCaptcha(context.Background(), ch.SessionID, "bad", browser)
	var retry *riot.CaptchaRetryError
	if !errors.As(err, &retry) {
		t.Fatalf("want CaptchaRetryError, got %v", err)
	}
	if retry.SiteKey != "k2" || retry.RQData != "d2" {
		t.Fatalf("retry %+v", retry)
	}
	if !errors.Is(err, riot.ErrPasswordCaptcha) {
		t.Fatalf("Is ErrPasswordCaptcha: %v", err)
	}
	deletedTDID := false
	for _, cookie := range retry.BrowserCookies {
		if cookie.Name == "tdid" && cookie.Value == "" && cookie.MaxAge < 0 && cookie.Domain == "riotgames.com" && cookie.Path == "/" {
			deletedTDID = true
		}
	}
	if !deletedTDID {
		t.Fatalf("retry browser cookies did not preserve tdid deletion: %#v", retry.BrowserCookies)
	}
	browser.Cookies = captchaCookieValues(retry.BrowserCookies)
	tok, mfa, err := c.CompleteCaptcha(context.Background(), ch.SessionID, "good", browser)
	if err != nil || mfa != nil {
		t.Fatalf("second complete: tok=%+v mfa=%v err=%v", tok, mfa, err)
	}
	if tok.AccessToken != "at-r" {
		t.Fatalf("%+v", tok)
	}
}
