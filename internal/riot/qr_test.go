package riot_test

import (
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

	"github.com/dosfsociety/valorant-bot/internal/riot"
)

// qrServer emulates the Riot Mobile QR login endpoints.
type qrServer struct {
	mu sync.Mutex

	polls          int
	pollsUntilDone int

	loginCookie string // Cookie header seen on POST /api/v1/login
	pollCookie  string // Cookie header seen on GET  /api/v1/login
	authzCookie string // Cookie header seen on POST /api/v1/authorization
	loginBody   map[string]any
	tokenBody   map[string]any
}

func newQRServer(t *testing.T) (*qrServer, *riot.QRClient) {
	t.Helper()
	qs := &qrServer{pollsUntilDone: 2}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "tdid", Value: "TDID"})
		_, _ = io.WriteString(w, `{"issuer":"https://auth.riotgames.com"}`)
	})
	mux.HandleFunc("POST /api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		qs.mu.Lock()
		qs.loginCookie = r.Header.Get("Cookie")
		_ = json.NewDecoder(r.Body).Decode(&qs.loginBody)
		qs.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "asid", Value: "ASID"})
		_, _ = io.WriteString(w, `{"type":"auth","cluster":"c1","suuid":"s1","timestamp":1700000000}`)
	})
	mux.HandleFunc("GET /api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		qs.mu.Lock()
		qs.polls++
		n := qs.polls
		qs.pollCookie = r.Header.Get("Cookie")
		qs.mu.Unlock()
		if n < qs.pollsUntilDone {
			_, _ = io.WriteString(w, `{"type":"auth","qrcode":{}}`)
			return
		}
		_, _ = io.WriteString(w, `{"type":"success","success":{"login_token":"LOGIN_TOKEN"}}`)
	})
	mux.HandleFunc("POST /api/v1/login-token", func(w http.ResponseWriter, r *http.Request) {
		qs.mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&qs.tokenBody)
		qs.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "ssid", Value: "SSID_VALUE"})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/authorization", func(w http.ResponseWriter, r *http.Request) {
		qs.mu.Lock()
		qs.authzCookie = r.Header.Get("Cookie")
		qs.mu.Unlock()
		_, _ = io.WriteString(w, `{"type":"response","response":{"parameters":{"uri":"http://localhost/redirect#access_token=ACCESS&id_token=ID&expires_in=3600"}}}`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := riot.NewQRClient(srv.Client())
	c.AuthBaseURL = srv.URL
	c.AuthenticateBaseURL = srv.URL
	c.QRLoginBaseURL = "https://qrlogin.example"
	return qs, c
}

func TestQRClient_HappyPath(t *testing.T) {
	qs, c := newQRServer(t)
	ctx := context.Background()

	sess, err := c.StartQRSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sess.SUUID != "s1" {
		t.Fatalf("suuid = %q", sess.SUUID)
	}
	for _, want := range []string{"https://qrlogin.example/riotmobile?", "cluster=c1", "suuid=s1", "timestamp=1700000000"} {
		if !strings.Contains(sess.LoginURL, want) {
			t.Fatalf("login URL %q missing %q", sess.LoginURL, want)
		}
	}

	qs.mu.Lock()
	if _, ok := qs.loginBody["qrcode"]; !ok {
		t.Fatalf("login body missing qrcode: %v", qs.loginBody)
	}
	if qs.loginBody["client_id"] != "riot-client" {
		t.Fatalf("client_id = %v", qs.loginBody["client_id"])
	}
	if !strings.Contains(qs.loginCookie, "tdid=TDID") {
		t.Fatalf("login cookie = %q (openid-configuration cookies not forwarded)", qs.loginCookie)
	}
	qs.mu.Unlock()

	// First poll: user has not approved yet.
	if _, err := c.PollQRSession(ctx, sess); !errors.Is(err, riot.ErrQRPending) {
		t.Fatalf("first poll err = %v, want ErrQRPending", err)
	}

	loginToken, err := c.PollQRSession(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	if loginToken != "LOGIN_TOKEN" {
		t.Fatalf("login token = %q", loginToken)
	}
	qs.mu.Lock()
	if !strings.Contains(qs.pollCookie, "asid=ASID") {
		t.Fatalf("poll cookie = %q (asid not carried)", qs.pollCookie)
	}
	qs.mu.Unlock()

	tokens, err := c.ExchangeLoginToken(ctx, loginToken)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "ACCESS" || tokens.IDToken != "ID" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if tokens.SessionCookie != "ssid=SSID_VALUE" {
		t.Fatalf("session cookie = %q", tokens.SessionCookie)
	}
	qs.mu.Lock()
	if qs.tokenBody["login_token"] != "LOGIN_TOKEN" {
		t.Fatalf("login-token body = %v", qs.tokenBody)
	}
	if qs.tokenBody["persist_login"] != true {
		t.Fatalf("persist_login must be true so /shop can reuse the ssid session: %v", qs.tokenBody)
	}
	if !strings.Contains(qs.authzCookie, "ssid=SSID_VALUE") {
		t.Fatalf("authorization cookie = %q", qs.authzCookie)
	}
	qs.mu.Unlock()
}

func TestQRClient_PollExpired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"error","error":"session_expired"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := riot.NewQRClient(srv.Client())
	c.AuthenticateBaseURL = srv.URL

	_, err := c.PollQRSession(context.Background(), &riot.QRSession{})
	if !errors.Is(err, riot.ErrQRExpired) {
		t.Fatalf("err = %v, want ErrQRExpired", err)
	}
}

func TestQRClient_StartRejectsMissingSUUID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"auth"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := riot.NewQRClient(srv.Client())
	c.AuthBaseURL = srv.URL
	c.AuthenticateBaseURL = srv.URL

	if _, err := c.StartQRSession(context.Background()); err == nil {
		t.Fatal("expected error when Riot returns no QR session")
	}
}

func TestQRClient_ExchangeRejectsBadStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/login-token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := riot.NewQRClient(srv.Client())
	c.AuthBaseURL = srv.URL

	if _, err := c.ExchangeLoginToken(context.Background(), "tok"); err == nil {
		t.Fatal("expected error on non-204 login-token response")
	}
}

func TestQRClient_ExchangeDoesNotFollowRedirectWithLoginToken(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(target.Close)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/login-token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", target.URL+"/stolen-login-token")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	c := riot.NewQRClient(source.Client())
	c.AuthBaseURL = source.URL
	if _, err := c.ExchangeLoginToken(context.Background(), "sensitive-login-token"); err == nil {
		t.Fatal("redirected login-token exchange unexpectedly succeeded")
	}
	if got := redirected.Load(); got != 0 {
		t.Fatalf("redirect target requests=%d, want 0", got)
	}
}
