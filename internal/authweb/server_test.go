package authweb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

type mockStore struct {
	pending  map[string]string
	accounts []store.Account
}

func newMockStore() *mockStore {
	return &mockStore{pending: make(map[string]string)}
}

func (m *mockStore) PutAuthPending(state, discordUserID string, expiresAt time.Time) error {
	m.pending[state] = discordUserID
	return nil
}

func (m *mockStore) TakeAuthPending(state string) (string, bool, error) {
	uid, ok := m.pending[state]
	if !ok {
		return "", false, nil
	}
	delete(m.pending, state)
	return uid, true, nil
}

func (m *mockStore) UpsertRiotAccount(a store.Account) error {
	m.accounts = append(m.accounts, a)
	return nil
}

type mockRiot struct {
	entitlements string
	puuid        string
	names        []riot.PlayerName
	namesErr     error
}

func (m *mockRiot) GetEntitlements(ctx context.Context, accessToken string) (string, error) {
	return m.entitlements, nil
}

func (m *mockRiot) GetUserInfo(ctx context.Context, accessToken string) (string, error) {
	return m.puuid, nil
}

func (m *mockRiot) GetPlayerNames(ctx context.Context, accessToken, entitlementsToken, shard string, puuids []string) ([]riot.PlayerName, error) {
	if m.namesErr != nil {
		return nil, m.namesErr
	}
	return m.names, nil
}

type mockBoxer struct {
	lastPlain []byte
}

func (m *mockBoxer) Encrypt(plain []byte) ([]byte, error) {
	m.lastPlain = append([]byte(nil), plain...)
	return []byte("enc"), nil
}

func testDeps(st Store, r RiotClient, b Boxer) Deps {
	return Deps{
		AuthBaseURL: "http://127.0.0.1:8787",
		Store:       st,
		Riot:        r,
		Boxer:       b,
	}
}

func jwtAccessToken(region string) string {
	payload, _ := json.Marshal(map[string]any{
		"dat": map[string]string{"r": region},
	})
	return "hdr." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func jwtIDToken(game, tag string) string {
	payload, _ := json.Marshal(map[string]any{
		"acct": map[string]string{"game_name": game, "tag_line": tag},
	})
	return "hdr." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestBeginAuth(t *testing.T) {
	st := newMockStore()
	s := New(testDeps(st, &mockRiot{}, &mockBoxer{}))
	loginURL, state, err := s.BeginAuth("d1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loginURL, "/login?state=") || state == "" {
		t.Fatalf("%s %s", loginURL, state)
	}
}

func TestLogin_AutoFlow(t *testing.T) {
	s := New(testDeps(newMockStore(), &mockRiot{}, &mockBoxer{}))
	req := httptest.NewRequest(http.MethodGet, "/login?state=abc-state", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "auth.riotgames.com/authorize") {
		t.Fatalf("missing riot link: %s", body)
	}
	if !strings.Contains(body, "client_id=riot-client") {
		t.Fatalf("expected riot-client: %s", body)
	}
	if !strings.Contains(body, "abc-state") {
		t.Fatalf("missing state: %s", body)
	}
	if !strings.Contains(body, "catcher-ping") {
		t.Fatal("missing catcher detection")
	}
	if !strings.Contains(body, "/api/auth/wait") {
		t.Fatal("missing wait poll")
	}
	if !strings.Contains(body, "install-catcher.sh") {
		t.Fatal("missing catcher install hint")
	}
	if strings.Contains(body, "붙여넣") && strings.Contains(body, "<form") {
		t.Fatal("paste form should be removed")
	}
}

func TestWait_AndInstallCatcher(t *testing.T) {
	st := newMockStore()
	s := New(testDeps(st, &mockRiot{}, &mockBoxer{}))
	_, state, err := s.BeginAuth("d1")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/wait?state="+url.QueryEscape(state), nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("wait status %d", rec.Code)
	}
	var pending authOutcome
	if err := json.NewDecoder(rec.Body).Decode(&pending); err != nil {
		t.Fatal(err)
	}
	if pending.Done {
		t.Fatal("expected not done yet")
	}

	s.setOutcome(state, authOutcome{Done: true, OK: true, Display: "A#B"})
	req2 := httptest.NewRequest(http.MethodGet, "/api/auth/wait?state="+url.QueryEscape(state), nil)
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	var done authOutcome
	if err := json.NewDecoder(rec2.Body).Decode(&done); err != nil {
		t.Fatal(err)
	}
	if !done.Done || !done.OK || done.Display != "A#B" {
		t.Fatalf("%+v", done)
	}

	req3 := httptest.NewRequest(http.MethodGet, "/install-catcher.sh", nil)
	rec3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("install status %d", rec3.Code)
	}
	script := rec3.Body.String()
	if !strings.Contains(script, "127.0.0.1:8787") || !strings.Contains(script, "catcher-ping") {
		t.Fatalf("bad install script: %s", script)
	}
}

func TestRedirectCatcher_HTML(t *testing.T) {
	s := New(testDeps(newMockStore(), &mockRiot{}, &mockBoxer{}))
	req := httptest.NewRequest(http.MethodGet, "/redirect", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "127.0.0.1:8787/api/auth/callback") {
		t.Fatalf("body missing callback: %s", body)
	}
	if !strings.Contains(body, "access_token") {
		t.Fatal("missing access_token handling")
	}
}

func TestCallback_AutoSave(t *testing.T) {
	st := newMockStore()
	st.pending["st"] = "discord-9"
	box := &mockBoxer{}
	ri := &mockRiot{
		entitlements: "ent",
		puuid:        "puuid-1",
		names:        []riot.PlayerName{{GameName: "Ace", TagLine: "KR1"}},
	}
	var linkedUser, linkedName string
	s := New(Deps{
		AuthBaseURL: "http://127.0.0.1:8787",
		Store:       st,
		Riot:        ri,
		Boxer:       box,
		OnLinked: func(uid, name string) {
			linkedUser, linkedName = uid, name
		},
	})

	access := jwtAccessToken("kr")
	id := jwtIDToken("Ace", "KR1")
	redirectURL := "http://localhost/redirect#access_token=" + access + "&id_token=" + id + "&state=st"

	form := url.Values{"state": {"st"}, "redirect_url": {redirectURL}}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "Ace#KR1") {
		t.Fatalf("%s", body)
	}
	if linkedUser != "discord-9" || linkedName != "Ace#KR1" {
		t.Fatalf("onLinked %s %s", linkedUser, linkedName)
	}
	if !strings.HasPrefix(string(box.lastPlain), "access_token=") {
		t.Fatalf("%q", box.lastPlain)
	}
}

func TestCallback_FallsBackToIDTokenName(t *testing.T) {
	st := newMockStore()
	st.pending["st"] = "d1"
	ri := &mockRiot{
		entitlements: "ent",
		puuid:        "p1",
		namesErr:     context.Canceled,
	}
	s := New(testDeps(st, ri, &mockBoxer{}))
	access := jwtAccessToken("kr")
	id := jwtIDToken("FromJWT", "1902")
	redirectURL := "http://localhost/redirect#access_token=" + access + "&id_token=" + id + "&state=st"
	form := url.Values{"state": {"st"}, "redirect_url": {redirectURL}}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "FromJWT#1902") {
		t.Fatalf("%s", rec.Body.String())
	}
}
