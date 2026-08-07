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

func TestPasswordBeginAndCompleteCaptcha_UsesBrowserSession(t *testing.T) {
	var puts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != captchaBrowserUserAgent {
			t.Fatalf("User-Agent = %q, want %q", got, captchaBrowserUserAgent)
		}
		switch r.Method {
		case http.MethodPost:
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

func TestPasswordBeginCaptchaSeedsRiotSessionFromDiscovery(t *testing.T) {
	var discoveryCalls atomic.Int32
	var loginSawDiscoveryCookie atomic.Bool
	var loginSawOutOfScopeCookie atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		discoveryCalls.Add(1)
		http.SetCookie(w, &http.Cookie{
			Name:     "__cf_bm",
			Value:    "cloudflare-session",
			Domain:   "riotgames.com",
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteNoneMode,
		})
		http.SetCookie(w, &http.Cookie{Name: "tracking", Value: "not-allowlisted", Domain: "riotgames.com", Path: "/", Secure: true})
		http.SetCookie(w, &http.Cookie{Name: "tdid", Value: "host-only", Path: "/", Secure: true, HttpOnly: true})
		http.SetCookie(w, &http.Cookie{Name: "authenticator.sid", Value: "wrong-path", Domain: "riotgames.com", Path: "/discovery-only", Secure: true, HttpOnly: true})
		http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "not-secure", Domain: "riotgames.com", Path: "/", HttpOnly: true})
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": "riot"})
	})
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("__cf_bm"); err == nil && cookie.Value == "cloudflare-session" {
			loginSawDiscoveryCookie.Store(true)
		}
		for _, name := range []string{"tracking", "tdid", "authenticator.sid", "__cflb"} {
			if _, err := r.Cookie(name); err == nil {
				loginSawOutOfScopeCookie.Store(true)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"captcha": map[string]any{
				"hcaptcha": map[string]string{"key": "site-key", "data": "rq-data"},
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
	challenge, err := c.BeginCaptcha(context.Background(), "user", "secret", riot.CaptchaBrowserSession{
		UserAgent: captchaBrowserUserAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := discoveryCalls.Load(); got != 1 {
		t.Fatalf("discovery calls = %d, want 1 before captcha login", got)
	}
	if !loginSawDiscoveryCookie.Load() {
		t.Fatal("captcha login did not receive the Riot discovery session cookie")
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
			t.Fatalf("browser received out-of-scope discovery cookie: %#v", cookie)
		}
	}
	if browserCookie == nil {
		t.Fatal("browser did not receive the discovery __cf_bm cookie")
	}
	if browserCookie.Domain != "riotgames.com" || browserCookie.Path != "/" ||
		!browserCookie.Secure || !browserCookie.HttpOnly || browserCookie.SameSite != http.SameSiteNoneMode {
		t.Fatalf("browser discovery cookie attributes changed: %#v", browserCookie)
	}
}

func TestPasswordCompleteCaptchaRejectsDifferentBrowserSession(t *testing.T) {
	var puts atomic.Int32
	mux := http.NewServeMux()
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
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != captchaBrowserUserAgent {
			t.Fatalf("authenticate User-Agent = %q, want %q", got, captchaBrowserUserAgent)
		}
		switch r.Method {
		case http.MethodPost:
			beginBaggage = r.Header.Get("baggage")
			if !strings.HasPrefix(beginBaggage, "sdksid=") || strings.TrimPrefix(beginBaggage, "sdksid=") == "" {
				t.Fatalf("captcha begin baggage = %q, want a non-empty sdksid", beginBaggage)
			}
			http.SetCookie(w, &http.Cookie{Name: "authenticator.sid", Value: "s1"})
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
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != captchaBrowserUserAgent {
			t.Fatalf("authenticate User-Agent = %q, want %q", got, captchaBrowserUserAgent)
		}
		switch r.Method {
		case http.MethodPost:
			sessionBaggage = r.Header.Get("baggage")
			if !strings.HasPrefix(sessionBaggage, "sdksid=") || strings.TrimPrefix(sessionBaggage, "sdksid=") == "" {
				t.Fatalf("captcha begin baggage = %q, want a non-empty sdksid", sessionBaggage)
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

func TestPasswordSubmitMFADoesNotRetryAfterLoginTokenExchangeFailure(t *testing.T) {
	var mfaPUTs atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
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
		if r.URL.Path == "/.well-known/openid-configuration" {
			return jsonResponse(r, `{}`), nil
		}
		if r.URL.Path != "/api/v1/login" {
			return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		switch r.Method {
		case http.MethodPost:
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

func TestPasswordSubmitMFAFallsBackAfterExplicitSchemaRejection(t *testing.T) {
	var canonicalPUTs atomic.Int32
	var alternatePUTs atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
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
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
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
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
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
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			sessionBaggage = r.Header.Get("baggage")
			if !strings.HasPrefix(sessionBaggage, "sdksid=") || strings.TrimPrefix(sessionBaggage, "sdksid=") == "" {
				t.Fatalf("captcha begin baggage = %q, want a non-empty sdksid", sessionBaggage)
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
