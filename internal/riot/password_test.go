package riot_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
			if _, err := c.BeginCaptcha(context.Background(), "user", "pass"); err != nil {
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
			if _, err := c.BeginCaptcha(context.Background(), "user", "pass"); err != nil {
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
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
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
			if cookie, err := r.Cookie("authenticator.sid"); err != nil || cookie.Value != "s1" {
				t.Fatalf("captcha completion missing begin-session cookie: cookie=%v err=%v", cookie, err)
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"user1"`) || !strings.Contains(string(body), "hcaptcha tok") {
				t.Fatalf("body %s", body)
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
	ch, err := c.BeginCaptcha(context.Background(), "user1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if ch.SiteKey != "site-key" || ch.RQData != "rq-data" || ch.SessionID == "" {
		t.Fatalf("%+v", ch)
	}
	tok, mfa, err := c.CompleteCaptcha(context.Background(), ch.SessionID, "tok")
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

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthenticateBaseURL: srv.URL}
	ch, err := c.BeginCaptcha(context.Background(), "user", "secret")
	if err != nil {
		t.Fatal(err)
	}
	c.CancelCaptcha(ch.SessionID)
	_, _, err = c.CompleteCaptcha(context.Background(), ch.SessionID, "token")
	if !errors.Is(err, riot.ErrCaptchaSession) {
		t.Fatalf("completion after cancel = %v, want ErrCaptchaSession", err)
	}
}

func TestPasswordCompleteCaptcha_MFA(t *testing.T) {
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
	ch, err := c.BeginCaptcha(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	_, mfa, err := c.CompleteCaptcha(context.Background(), ch.SessionID, "tok")
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
		AuthenticateBaseURL: srv.URL,
	}
	ch, err := c.BeginCaptcha(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.CompleteCaptcha(context.Background(), ch.SessionID, "tok")
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
		AuthenticateBaseURL: srv.URL,
	}
	ch, err := c.BeginCaptcha(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.CompleteCaptcha(context.Background(), ch.SessionID, "tok")
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
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "auth",
				"captcha": map[string]any{
					"hcaptcha": map[string]string{"key": "k1", "data": "d1"},
				},
			})
		case http.MethodPut:
			puts++
			if puts == 1 {
				http.SetCookie(w, &http.Cookie{Name: "retry.sid", Value: "retry-session"})
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
			if cookie, err := r.Cookie("retry.sid"); err != nil || cookie.Value != "retry-session" {
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
	ch, err := c.BeginCaptcha(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.CompleteCaptcha(context.Background(), ch.SessionID, "bad")
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
	tok, mfa, err := c.CompleteCaptcha(context.Background(), ch.SessionID, "good")
	if err != nil || mfa != nil {
		t.Fatalf("second complete: tok=%+v mfa=%v err=%v", tok, mfa, err)
	}
	if tok.AccessToken != "at-r" {
		t.Fatalf("%+v", tok)
	}
}
