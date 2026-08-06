package authweb

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListenLocalhost_ServesRedirect(t *testing.T) {
	s := New(Deps{AuthBaseURL: "http://127.0.0.1:8787"})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /catcher-ping", s.handleCatcherPing)
	mux.HandleFunc("GET /redirect", s.handleRedirectCatcher)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/catcher-ping")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}

	res2, err := http.Get(srv.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	b, _ := io.ReadAll(res2.Body)
	body := string(b)
	if !strings.Contains(body, "/api/auth/callback") {
		t.Fatalf("redirect page missing callback: %s", body)
	}
}
