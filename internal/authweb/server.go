package authweb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

const defaultPendingTTL = 15 * time.Minute

// RiotRedirectURI is the OAuth redirect Riot returns to after login (riot-client).
// Must be served at http://localhost/redirect (port 80).
const RiotRedirectURI = "http://localhost/redirect"

// Store persists auth pending state and Riot accounts.
type Store interface {
	PutAuthPending(state, discordUserID string, expiresAt time.Time) error
	TakeAuthPending(state string) (discordUserID string, ok bool, err error)
	UpsertRiotAccount(a store.Account) error
}

// RiotClient talks to Riot auth / name APIs.
type RiotClient interface {
	GetEntitlements(ctx context.Context, accessToken string) (string, error)
	GetUserInfo(ctx context.Context, accessToken string) (string, error)
	GetPlayerNames(ctx context.Context, accessToken, entitlementsToken, shard string, puuids []string) ([]riot.PlayerName, error)
}

// Boxer encrypts session material at rest.
type Boxer interface {
	Encrypt(plain []byte) ([]byte, error)
}

// LinkedNotifier is called after a Riot account is linked (e.g. Discord DM).
type LinkedNotifier func(discordUserID, displayName string)

// Deps configures the auth web server.
type Deps struct {
	AuthBaseURL string
	PendingTTL  time.Duration
	Store       Store
	Riot        RiotClient
	Boxer       Boxer
	OnLinked    LinkedNotifier
}

// Server serves login redirect + Riot callback catcher.
type Server struct {
	authBaseURL string
	pendingTTL  time.Duration
	store       Store
	riot        RiotClient
	boxer       Boxer
	onLinked    LinkedNotifier
	mux         *http.ServeMux
}

// New builds an auth web Server.
func New(d Deps) *Server {
	ttl := d.PendingTTL
	if ttl <= 0 {
		ttl = defaultPendingTTL
	}
	s := &Server{
		authBaseURL: strings.TrimRight(d.AuthBaseURL, "/"),
		pendingTTL:  ttl,
		store:       d.Store,
		riot:        d.Riot,
		boxer:       d.Boxer,
		onLinked:    d.OnLinked,
		mux:         http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("GET /redirect", s.handleRedirectCatcher)
	s.mux.HandleFunc("POST /api/auth/callback", s.handleCallback)
	s.mux.HandleFunc("OPTIONS /api/auth/callback", s.handleCallbackCORS)
	s.mux.HandleFunc("GET /", s.handleIndex)
	return s
}

// Handler returns the HTTP handler (AUTH_PORT).
func (s *Server) Handler() http.Handler {
	return s.mux
}

// RedirectHandler serves only GET /redirect for the port-80 Riot callback.
func (s *Server) RedirectHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /redirect", s.handleRedirectCatcher)
	mux.HandleFunc("/redirect", s.handleRedirectCatcher) // fallback for older mux matching
	return mux
}

// BeginAuth creates a pending auth state and returns the Discord login button URL.
func (s *Server) BeginAuth(discordUserID string) (loginURL, state string, err error) {
	state, err = newState()
	if err != nil {
		return "", "", err
	}
	expiresAt := time.Now().Add(s.pendingTTL)
	if err := s.store.PutAuthPending(state, discordUserID, expiresAt); err != nil {
		return "", "", err
	}
	loginURL = s.authBaseURL + "/login?state=" + url.QueryEscape(state)
	return loginURL, state, nil
}

func riotAuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", "riot-client")
	q.Set("redirect_uri", RiotRedirectURI)
	q.Set("response_type", "token id_token")
	q.Set("nonce", "1")
	q.Set("scope", "account openid")
	q.Set("state", state)
	return "https://auth.riotgames.com/authorize?" + q.Encode()
}

// CompleteFromRedirectURL links a Riot account from an OAuth redirect URL.
func (s *Server) CompleteFromRedirectURL(ctx context.Context, state, redirectURL, regionFallback string) (displayName string, err error) {
	if regionFallback == "" {
		regionFallback = "kr"
	}
	discordUserID, ok, err := s.store.TakeAuthPending(state)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("unknown or expired state")
	}

	accessToken, idToken, err := riot.ParseRedirectURL(redirectURL)
	if err != nil {
		return "", err
	}

	entitlements, err := s.riot.GetEntitlements(ctx, accessToken)
	if err != nil {
		return "", fmt.Errorf("entitlements: %w", err)
	}

	puuid, err := s.riot.GetUserInfo(ctx, accessToken)
	if err != nil {
		return "", fmt.Errorf("userinfo: %w", err)
	}

	region, shard, err := riot.RegionFromToken(accessToken)
	if err != nil {
		region = regionFallback
		shard, err = riot.ShardForRegion(region)
		if err != nil {
			return "", err
		}
	}

	gameName, tagLine := "", ""
	if names, nerr := s.riot.GetPlayerNames(ctx, accessToken, entitlements, shard, []string{puuid}); nerr == nil && len(names) > 0 {
		gameName = names[0].GameName
		tagLine = names[0].TagLine
	} else if idToken != "" {
		if gn, tl, ierr := riot.GameNameFromIDToken(idToken); ierr == nil {
			gameName, tagLine = gn, tl
		} else if nerr != nil {
			return "", fmt.Errorf("player names: %w", nerr)
		}
	} else if nerr != nil {
		return "", fmt.Errorf("player names: %w", nerr)
	}

	cipherText, err := s.boxer.Encrypt([]byte("access_token=" + accessToken))
	if err != nil {
		return "", err
	}

	acc := store.Account{
		DiscordUserID:     discordUserID,
		PUUID:             puuid,
		GameName:          gameName,
		TagLine:           tagLine,
		Region:            region,
		Shard:             shard,
		CookiesCiphertext: cipherText,
	}
	if err := s.store.UpsertRiotAccount(acc); err != nil {
		return "", err
	}

	display := gameName + "#" + tagLine
	if s.onLinked != nil {
		s.onLinked(discordUserID, display)
	}
	return display, nil
}

func newState() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// handleIndex explains how remote clients should authenticate.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

// handleLogin shows Riot login + paste form (required for other computers).
// Riot always redirects to http://localhost/redirect on the *browser* machine,
// so remote users must paste that URL back to this server.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		http.Error(w, "missing state — Discord에서 /auth 를 다시 실행하세요.", http.StatusBadRequest)
		return
	}
	riotURL := riotAuthorizeURL(state)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, loginPageHTML,
		html.EscapeString(riotURL),
		html.EscapeString(s.authBaseURL+"/api/auth/callback"),
		html.EscapeString(state),
		html.EscapeString(s.authBaseURL),
	)
}

// handleRedirectCatcher runs at http://localhost/redirect after Riot login.
// JS reads the hash and POSTs tokens to the bot callback automatically.
func (s *Server) handleRedirectCatcher(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, redirectCatcherHTML, s.authBaseURL+"/api/auth/callback", s.authBaseURL)
}

func (s *Server) handleCallbackCORS(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	state := strings.TrimSpace(r.FormValue("state"))
	redirectURL := strings.TrimSpace(r.FormValue("redirect_url"))
	region := strings.TrimSpace(r.FormValue("region"))
	if redirectURL == "" {
		http.Error(w, "redirect_url is required", http.StatusBadRequest)
		return
	}

	display, err := s.CompleteFromRedirectURL(r.Context(), state, redirectURL, region)
	if err != nil {
		log.Printf("auth callback error: %v", err)
		msg := err.Error()
		code := http.StatusBadRequest
		if strings.Contains(msg, "entitlements") || strings.Contains(msg, "userinfo") || strings.Contains(msg, "player names") {
			code = http.StatusBadGateway
		}
		http.Error(w, msg, code)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, successHTML, html.EscapeString(display))
}

const redirectCatcherHTML = `<!DOCTYPE html>
<html lang="ko">
<head>
<meta charset="utf-8">
<title>연동 처리 중…</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 36rem; margin: 2rem auto; padding: 0 1rem; line-height: 1.45; }
  code, textarea { word-break: break-all; }
  textarea { width: 100%%; min-height: 5rem; }
</style>
</head>
<body>
<p id="msg">Discord 계정에 연결하는 중…</p>
<div id="fallback" style="display:none">
  <p>자동 저장에 실패했습니다. 아래 주소창 URL 전체를 복사한 뒤,
     봇 서버의 연동 페이지에 붙여넣으세요.</p>
  <p>연동 페이지: <a id="loginHint" href="#"></a></p>
  <textarea id="urlBox" readonly></textarea>
</div>
<script>
(function () {
  var callback = %q;
  var authBase = %q;
  var hash = (window.location.hash || "").replace(/^#/, "");
  var params = new URLSearchParams(hash);
  var access = params.get("access_token");
  var state = params.get("state");
  var msg = document.getElementById("msg");
  if (!access) {
    msg.textContent = "access_token을 찾지 못했습니다. Discord에서 /auth 를 다시 실행해 주세요.";
    return;
  }
  var body = new URLSearchParams();
  body.set("state", state || "");
  body.set("redirect_url", window.location.href);
  fetch(callback, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: body.toString(),
    mode: "cors"
  }).then(function (res) {
    return res.text().then(function (text) {
      if (!res.ok) throw new Error(text || ("HTTP " + res.status));
      document.open();
      document.write(text);
      document.close();
    });
  }).catch(function (err) {
    msg.textContent = "저장 실패: " + err.message;
    var fb = document.getElementById("fallback");
    fb.style.display = "block";
    document.getElementById("urlBox").value = window.location.href;
    var a = document.getElementById("loginHint");
    a.href = authBase + "/login?state=" + encodeURIComponent(state || "");
    a.textContent = authBase + "/login?state=…";
  });
})();
</script>
</body>
</html>
`

const loginPageHTML = `<!DOCTYPE html>
<html lang="ko">
<head>
<meta charset="utf-8">
<title>Riot 계정 연동</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 36rem; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
  .btn { display: inline-block; background: #fd4553; color: #fff; padding: .7rem 1.2rem; border-radius: 8px; text-decoration: none; font-weight: 600; }
  textarea { width: 100%%; min-height: 6rem; font-family: ui-monospace, monospace; font-size: 12px; }
  .box { border: 1px solid #ddd; border-radius: 8px; padding: 1rem; margin-top: 1.5rem; background: #fafafa; }
  code { background: #eee; padding: 0 .25rem; }
  button { margin-top: .5rem; padding: .5rem 1rem; }
</style>
</head>
<body>
<h1>Riot 계정 연동</h1>
<p><a class="btn" href="%s">Riot으로 로그인</a></p>
<p>같은 PC에서 봇을 실행 중이면 로그인 후 자동으로 완료됩니다.</p>

<div class="box">
  <h2>다른 컴퓨터에서 로그인할 때</h2>
  <ol>
    <li>위 버튼으로 Riot 로그인</li>
    <li>끝나면 브라우저가 <code>http://localhost/redirect#access_token=…</code> 로 이동합니다
        (페이지가 안 열려도 <strong>주소창 URL 전체</strong>를 복사)</li>
    <li>그 URL을 아래에 붙여넣고 제출</li>
  </ol>
  <p>이 페이지 주소가 <code>127.0.0.1</code> 이면 다른 PC에서 접속할 수 없습니다.
     봇 서버의 <code>AUTH_BASE_URL</code>을 LAN IP로 바꾸세요. (예: <code>http://192.168.0.10:8787</code>)</p>
  <form method="POST" action="%s">
    <input type="hidden" name="state" value="%s">
    <label>localhost/redirect URL<br>
      <textarea name="redirect_url" placeholder="http://localhost/redirect#access_token=...&id_token=...&state=..." required></textarea>
    </label><br>
    <button type="submit">연동 완료</button>
  </form>
</div>
<p style="color:#666;font-size:.9rem">서버: %s</p>
<script>
  // Same-machine convenience: open Riot after a short delay unless user is already pasting.
  setTimeout(function () {
    if (document.activeElement && document.activeElement.tagName === "TEXTAREA") return;
    // Do not auto-navigate on remote paste flow; user clicks the button.
  }, 0);
</script>
</body>
</html>
`

const indexHTML = `<!DOCTYPE html>
<html lang="ko">
<head><meta charset="utf-8"><title>Valorant Bot Auth</title></head>
<body style="font-family:system-ui;max-width:36rem;margin:2rem auto;padding:0 1rem">
<h1>Valorant Bot</h1>
<p>Discord에서 <code>/auth</code> 를 실행한 뒤 로그인 버튼을 누르세요.</p>
<p>다른 PC에서 연동하려면 <code>AUTH_BASE_URL</code>이 이 기기의 LAN IP여야 합니다.</p>
</body>
</html>
`

const successHTML = `<!DOCTYPE html>
<html lang="ko">
<head>
<meta charset="utf-8">
<title>연동 완료</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 28rem; margin: 3rem auto; padding: 0 1rem; text-align: center; }
</style>
</head>
<body>
<h1>연동 완료</h1>
<p>계정 <strong>%s</strong> 이(가) Discord에 연결되었습니다.</p>
<p>이 창을 닫고 Discord에서 <code>/shop</code> 또는 <code>/accounts</code>를 사용하세요.</p>
</body>
</html>
`
