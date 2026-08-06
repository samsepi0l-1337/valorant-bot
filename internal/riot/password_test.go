package riot_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dosfsociety/valorant-bot/internal/riot"
)

func TestPasswordLogin_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			http.SetCookie(w, &http.Cookie{Name: "asid", Value: "a1"})
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"user1"`) {
				t.Fatalf("body %s", body)
			}
			http.SetCookie(w, &http.Cookie{Name: "ssid", Value: "session-cookie"})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "response",
				"response": map[string]any{
					"parameters": map[string]any{
						"uri": "http://localhost/redirect#access_token=at-pass&id_token=id-pass",
					},
				},
			})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL}
	tok, mfa, err := c.LoginWithPassword(context.Background(), "user1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if mfa != nil {
		t.Fatal("unexpected mfa")
	}
	if tok.AccessToken != "at-pass" || tok.SessionCookie != "ssid=session-cookie" {
		t.Fatalf("%+v", tok)
	}
}

func TestPasswordLogin_MFA(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"type":"multifactor"`) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"type": "response",
					"response": map[string]any{
						"parameters": map[string]any{
							"uri": "http://localhost/redirect#access_token=at-mfa&id_token=id-mfa",
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "multifactor",
				"multifactor": map[string]any{
					"email":   "a***@ex.com",
					"method":  "email",
					"methods": []string{"email", "authenticator"},
				},
			})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL}
	_, mfa, err := c.LoginWithPassword(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	if mfa == nil || mfa.Email != "a***@ex.com" || mfa.Method != "email" {
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

func TestPasswordLogin_MFAAuthenticator(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "multifactor",
				"multifactor": map[string]any{
					"method":  "authenticator",
					"methods": []string{"authenticator", "email"},
				},
			})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL}
	_, mfa, err := c.LoginWithPassword(context.Background(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	if mfa == nil || mfa.Method != "authenticator" {
		t.Fatalf("mfa %+v", mfa)
	}
}

func TestPasswordLogin_InvalidCodeKeepsMFA(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"type":"multifactor"`) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"type":  "multifactor",
					"error": "invalid_code",
					"multifactor": map[string]any{
						"email":  "a***@ex.com",
						"method": "email",
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "multifactor",
				"multifactor": map[string]any{
					"email":  "a***@ex.com",
					"method": "email",
				},
			})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL}
	_, mfa, err := c.LoginWithPassword(context.Background(), "u", "p")
	if err != nil || mfa == nil {
		t.Fatalf("login mfa=%v err=%v", mfa, err)
	}
	_, err = c.SubmitMFA(context.Background(), mfa, "000000")
	if !errors.Is(err, riot.ErrPasswordInvalidCode) {
		t.Fatalf("err=%v", err)
	}
}

func TestPasswordLogin_AuthFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/authorization", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusOK)
		case http.MethodPut:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":  "auth",
				"error": "auth_failure",
			})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &riot.PasswordClient{HTTPClient: srv.Client(), AuthBaseURL: srv.URL}
	_, _, err := c.LoginWithPassword(context.Background(), "u", "p")
	if !errors.Is(err, riot.ErrPasswordInvalid) {
		t.Fatalf("err=%v", err)
	}
}
