package authweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
)

// ErrCaptchaOwner means a Discord user tried to control another user's login.
var ErrCaptchaOwner = errors.New("only the login owner can open this captcha")

type passwordPending struct {
	discordUserID string
	username      string
	password      string
	sessionID     string
	siteKey       string
	rqData        string
	challengeVer  uint64
	challengeWait chan struct{}
	flow          *passwordFlow
	expiresAt     time.Time
}

type passwordFlow struct {
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	launchMu sync.Mutex
	submitMu sync.Mutex
}

type passwordOutcome struct {
	done     bool
	display  string
	mfaState string
	mfaHint  string
	err      error
}

// BeginPasswordLogin stores credentials and starts the bot-host Chrome flow.
// captchaURL is intentionally empty: Discord uses a custom-ID button to ask the
// bot process to re-launch Chrome, never a localhost/public link on the user device.
func (s *Server) BeginPasswordLogin(ctx context.Context, discordUserID, username, password string) (captchaURL, state string, err error) {
	_ = ctx
	if s.passwordAuth == nil {
		return "", "", fmt.Errorf("password auth not configured")
	}
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return "", "", riot.ErrPasswordInvalid
	}
	state, err = newState()
	if err != nil {
		return "", "", err
	}
	flowCtx, flowCancel := context.WithCancel(context.Background())
	flow := &passwordFlow{ctx: flowCtx, cancel: flowCancel}
	flow.wg.Add(1)
	s.mu.Lock()
	if s.passwordPending == nil {
		s.passwordPending = make(map[string]passwordPending)
	}
	if s.passwordOutcomes == nil {
		s.passwordOutcomes = make(map[string]passwordOutcome)
	}
	if s.passwordReady == nil {
		s.passwordReady = make(map[string]chan struct{})
	}
	s.passwordPending[state] = passwordPending{
		discordUserID: discordUserID,
		username:      username,
		password:      password,
		flow:          flow,
		expiresAt:     time.Now().Add(s.pendingTTL),
	}
	s.passwordOutcomes[state] = passwordOutcome{}
	s.passwordReady[state] = make(chan struct{})
	s.mu.Unlock()
	go s.expirePasswordState(state)

	// Prefetch the single Riot session and open Chrome in the background so the
	// Discord modal can be acknowledged without waiting on Riot/network startup.
	go func() {
		defer flow.wg.Done()
		s.prepareCaptchaSession(state)
	}()

	return "", state, nil
}

func (s *Server) prepareCaptchaSession(state string) {
	s.mu.Lock()
	pending, ok := s.passwordPending[state]
	s.mu.Unlock()
	if !ok || pending.flow == nil {
		return
	}
	ctx, cancel := context.WithTimeout(pending.flow.ctx, 40*time.Second)
	defer cancel()
	if _, err := s.ensureCaptchaChallenge(ctx, state); err != nil {
		log.Printf("captcha prefetch state=%s: %v", state, err)
		s.setPasswordOutcome(state, passwordOutcome{err: err})
		return
	}
	if err := s.waitCaptchaTLS(3 * time.Second); err != nil {
		log.Printf("captcha tls wait state=%s: %v", state, err)
		s.setPasswordOutcome(state, passwordOutcome{err: err})
		return
	}
	if err := s.ensureCaptchaLaunched(state); err != nil {
		log.Printf("captcha auto-launch state=%s: %v", state, err)
		s.setPasswordOutcome(state, passwordOutcome{err: err})
	}
}

// LaunchPasswordCaptcha validates the Discord owner and asks the bot host to
// open a fresh Chrome window. It never exposes loopback URLs to Discord users.
func (s *Server) LaunchPasswordCaptcha(ctx context.Context, state, discordUserID string) error {
	state = strings.TrimSpace(state)
	discordUserID = strings.TrimSpace(discordUserID)
	pending, done, ok := s.beginPasswordOperation(state)
	if !ok {
		return fmt.Errorf("captcha session expired; run /auth again")
	}
	defer done()
	if pending.discordUserID != discordUserID {
		return ErrCaptchaOwner
	}
	if _, err := s.ensureCaptchaChallenge(ctx, state); err != nil {
		return err
	}
	if err := s.waitCaptchaTLS(3 * time.Second); err != nil {
		return err
	}
	s.resetCaptchaLaunch(state)
	return s.ensureCaptchaLaunched(state)
}

// WaitPasswordLogin blocks until the captcha page completes (success, MFA, or terminal error).
// Captcha retries (new widget challenge) do not finish this wait.
func (s *Server) WaitPasswordLogin(ctx context.Context, state string) (displayName, mfaState, mfaHint string, err error) {
	ticker := time.NewTicker(s.qrPollInterval)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		out, ok := s.passwordOutcomes[state]
		ready := s.passwordReady[state]
		s.mu.Unlock()
		if ok && out.done {
			s.cleanupPasswordState(state)
			return out.display, out.mfaState, out.mfaHint, out.err
		}
		select {
		case <-ctx.Done():
			s.cleanupPasswordState(state)
			return "", "", "", ctx.Err()
		case <-ready:
		case <-ticker.C:
		}
	}
}

func (s *Server) setPasswordOutcome(state string, out passwordOutcome) {
	out.done = true
	s.mu.Lock()
	if _, ok := s.passwordPending[state]; !ok {
		s.mu.Unlock()
		return
	}
	if current, ok := s.passwordOutcomes[state]; ok && current.done {
		s.mu.Unlock()
		return
	}
	s.passwordOutcomes[state] = out
	if ready := s.passwordReady[state]; ready != nil {
		close(ready)
		delete(s.passwordReady, state)
	}
	s.mu.Unlock()
}

func (s *Server) cleanupPasswordState(state string) {
	s.mu.Lock()
	pending, ok := s.passwordPending[state]
	delete(s.passwordPending, state)
	delete(s.passwordOutcomes, state)
	delete(s.passwordReady, state)
	delete(s.captchaLaunched, state)
	s.mu.Unlock()
	if ok && pending.flow != nil {
		pending.flow.cancel()
		pending.flow.launchMu.Lock()
		pending.flow.launchMu.Unlock()
		pending.flow.wg.Wait()
	}
	if ok && pending.sessionID != "" && s.passwordAuth != nil {
		s.passwordAuth.CancelCaptcha(pending.sessionID)
	}
}

func (s *Server) beginPasswordOperation(state string) (passwordPending, func(), bool) {
	s.mu.Lock()
	pending, ok := s.passwordPending[state]
	if !ok || pending.flow == nil || pending.flow.ctx.Err() != nil || time.Now().After(pending.expiresAt) {
		s.mu.Unlock()
		return passwordPending{}, nil, false
	}
	pending.flow.wg.Add(1)
	s.mu.Unlock()
	return pending, pending.flow.wg.Done, true
}

func passwordOperationContext(parent context.Context, flow *passwordFlow) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(flow.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func (s *Server) livePasswordState(state, sessionID string) (passwordPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.passwordPending[state]
	out := s.passwordOutcomes[state]
	if !ok || pending.flow == nil || pending.flow.ctx.Err() != nil ||
		time.Now().After(pending.expiresAt) || pending.sessionID != sessionID || out.done {
		return passwordPending{}, false
	}
	return pending, true
}

func (s *Server) expirePasswordState(state string) {
	for {
		s.mu.Lock()
		pending, ok := s.passwordPending[state]
		s.mu.Unlock()
		if !ok {
			return
		}
		wait := time.Until(pending.expiresAt)
		if wait <= 0 {
			s.cleanupPasswordState(state)
			return
		}
		timer := time.NewTimer(wait)
		<-timer.C
	}
}

func (s *Server) ensureCaptchaChallenge(ctx context.Context, state string) (passwordPending, error) {
	for {
		s.mu.Lock()
		pending, ok := s.passwordPending[state]
		if !ok || pending.flow == nil || pending.flow.ctx.Err() != nil || time.Now().After(pending.expiresAt) {
			s.mu.Unlock()
			return passwordPending{}, fmt.Errorf("captcha session expired; run /auth again")
		}
		if pending.sessionID != "" && pending.siteKey != "" && pending.rqData != "" {
			s.mu.Unlock()
			return pending, nil
		}
		if wait := pending.challengeWait; wait != nil {
			s.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-pending.flow.ctx.Done():
				return passwordPending{}, fmt.Errorf("captcha session expired; run /auth again")
			case <-ctx.Done():
				return passwordPending{}, ctx.Err()
			}
		}

		// Exactly one BeginCaptcha request may own a Discord auth state. Riot
		// binds rqdata to the cookies created by that request; allowing a second
		// request would hand the browser one challenge and submit it on another
		// session, which Riot rejects as invalid_request.
		wait := make(chan struct{})
		pending.challengeWait = wait
		s.passwordPending[state] = pending
		s.mu.Unlock()

		beginCtx, cancel := passwordOperationContext(ctx, pending.flow)
		ch, err := s.passwordAuth.BeginCaptcha(beginCtx, pending.username, pending.password)
		cancel()

		s.mu.Lock()
		cur, stillPending := s.passwordPending[state]
		if stillPending && cur.challengeWait == wait {
			cur.challengeWait = nil
			if err == nil {
				cur.sessionID = ch.SessionID
				cur.siteKey = ch.SiteKey
				cur.rqData = ch.RQData
				cur.challengeVer++
				cur.expiresAt = time.Now().Add(s.pendingTTL)
			}
			s.passwordPending[state] = cur
		}
		close(wait)
		s.mu.Unlock()

		if err != nil {
			return passwordPending{}, err
		}
		if !stillPending {
			if ch.SessionID != "" {
				s.passwordAuth.CancelCaptcha(ch.SessionID)
			}
			return passwordPending{}, fmt.Errorf("captcha session expired; run /auth again")
		}
		return cur, nil
	}
}

func (s *Server) handleCaptchaWidgetPage(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	s.mu.Lock()
	pending, ok := s.passwordPending[state]
	s.mu.Unlock()
	if !ok || time.Now().After(pending.expiresAt) {
		http.Error(w, "captcha session expired; run /auth again", http.StatusBadRequest)
		return
	}

	// Never render hCaptcha on a tunnel/loopback/legacy hostname. The token's
	// origin is part of Riot's validation and must match the authenticator UI.
	host := strings.ToLower(r.Host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if host != RiotCaptchaHost {
		log.Printf("captcha widget opened on unexpected host=%q (tokens will be rejected by Riot)", r.Host)
		http.Error(w, "captcha must open on "+RiotCaptchaHost, http.StatusBadRequest)
		return
	}

	stateJS, _ := json.Marshal(state)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, captchaWidgetHTML, stateJS)
}

func (s *Server) handleCaptchaChallenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	_, done, ok := s.beginPasswordOperation(state)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "captcha session expired; run /auth again"})
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	pending, err := s.ensureCaptchaChallenge(ctx, state)
	if err != nil {
		log.Printf("captcha challenge state=%s: %v", state, err)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"sitekey": pending.siteKey,
		"rqdata":  pending.rqData,
		"version": pending.challengeVer,
	})
}

func (s *Server) handleCaptchaSubmit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var req struct {
		State   string `json:"state"`
		Token   string `json:"token"`
		Version uint64 `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"ok":false,"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	req.State = strings.TrimSpace(req.State)
	req.Token = strings.TrimSpace(req.Token)

	pending, done, ok := s.beginPasswordOperation(req.State)
	if !ok {
		http.Error(w, `{"ok":false,"error":"expired"}`, http.StatusBadRequest)
		return
	}
	defer done()
	if pending.sessionID == "" {
		http.Error(w, `{"ok":false,"error":"captcha not prepared"}`, http.StatusBadRequest)
		return
	}
	if req.Token == "" {
		http.Error(w, `{"ok":false,"error":"missing token"}`, http.StatusBadRequest)
		return
	}
	pending.flow.submitMu.Lock()
	defer pending.flow.submitMu.Unlock()
	current, live := s.livePasswordState(req.State, pending.sessionID)
	if !live {
		http.Error(w, `{"ok":false,"error":"expired"}`, http.StatusBadRequest)
		return
	}
	pending = current
	if req.Version != pending.challengeVer {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      false,
			"retry":   true,
			"sitekey": pending.siteKey,
			"rqdata":  pending.rqData,
			"version": pending.challengeVer,
			"error":   "captcha challenge was replaced; solve the current checkbox",
		})
		return
	}

	requestCtx, requestCancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer requestCancel()
	ctx, cancel := passwordOperationContext(requestCtx, pending.flow)
	defer cancel()

	tokens, challenge, err := s.passwordAuth.CompleteCaptcha(ctx, pending.sessionID, req.Token)
	current, live = s.livePasswordState(req.State, pending.sessionID)
	if !live {
		s.passwordAuth.CancelCaptcha(pending.sessionID)
		http.Error(w, `{"ok":false,"error":"expired"}`, http.StatusBadRequest)
		return
	}
	pending = current
	if err != nil {
		var retry *riot.CaptchaRetryError
		if errors.As(err, &retry) {
			s.mu.Lock()
			p, stillLive := s.passwordPending[req.State]
			stillLive = stillLive && p.flow == pending.flow && p.flow.ctx.Err() == nil &&
				p.sessionID == pending.sessionID && time.Now().Before(p.expiresAt)
			if !stillLive {
				s.mu.Unlock()
				http.Error(w, `{"ok":false,"error":"expired"}`, http.StatusBadRequest)
				return
			}
			p.siteKey = retry.SiteKey
			p.rqData = retry.RQData
			p.challengeVer++
			p.expiresAt = time.Now().Add(s.pendingTTL)
			s.passwordPending[req.State] = p
			s.mu.Unlock()
			log.Printf("captcha retry state=%s reason=%s", req.State, retry.Reason)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      false,
				"retry":   true,
				"sitekey": retry.SiteKey,
				"rqdata":  retry.RQData,
				"version": p.challengeVer,
				"error":   retry.Reason,
			})
			return
		}
		if errors.Is(err, riot.ErrCaptchaSession) {
			s.mu.Lock()
			if p, ok := s.passwordPending[req.State]; ok && p.flow == pending.flow && p.sessionID == pending.sessionID {
				p.sessionID = ""
				p.siteKey = ""
				p.rqData = ""
				p.challengeVer++
				s.passwordPending[req.State] = p
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":    false,
				"error": "captcha session expired; reload this page",
			})
			return
		}
		log.Printf("captcha submit state=%s: %v", req.State, err)
		s.setPasswordOutcome(req.State, passwordOutcome{err: err})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if challenge != nil {
		mfaState, err := newState()
		if err != nil {
			s.setPasswordOutcome(req.State, passwordOutcome{err: err})
			http.Error(w, `{"ok":false,"error":"state"}`, http.StatusInternalServerError)
			return
		}
		s.mu.Lock()
		current, stillLive := s.passwordPending[req.State]
		if !stillLive || current.flow != pending.flow || current.flow.ctx.Err() != nil ||
			current.sessionID != pending.sessionID || !time.Now().Before(current.expiresAt) {
			s.mu.Unlock()
			http.Error(w, `{"ok":false,"error":"expired"}`, http.StatusBadRequest)
			return
		}
		mfaCtx, mfaCancel := context.WithCancel(context.Background())
		s.mfaPending[mfaState] = mfaPending{
			discordUserID: pending.discordUserID,
			challenge:     challenge,
			flow:          &mfaFlow{ctx: mfaCtx, cancel: mfaCancel},
			expiresAt:     time.Now().Add(s.pendingTTL),
		}
		s.mu.Unlock()
		go s.expireMFAState(mfaState)
		hint := formatMFAHint(challenge)
		s.setPasswordOutcome(req.State, passwordOutcome{mfaState: mfaState, mfaHint: hint})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "mfa": true})
		return
	}

	display, err := s.completePasswordTokens(ctx, pending.discordUserID, tokens)
	if err != nil {
		s.setPasswordOutcome(req.State, passwordOutcome{err: err})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.setPasswordOutcome(req.State, passwordOutcome{display: display})
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "mfa": false, "display": display})
}

func formatMFAHint(challenge *riot.MFAChallenge) string {
	if challenge == nil {
		return ""
	}
	if email := strings.TrimSpace(challenge.Email); email != "" {
		return email
	}
	method := strings.ToLower(strings.TrimSpace(challenge.Method))
	if method == "" {
		for _, m := range challenge.Methods {
			if method = strings.ToLower(strings.TrimSpace(m)); method != "" {
				break
			}
		}
	}
	switch method {
	case "email":
		return "email"
	case "authenticator", "otp", "otpauth", "riot_mobile", "mobile":
		return "authenticator"
	default:
		return method
	}
}

const captchaWidgetHTML = `<!DOCTYPE html>
<html lang="ko">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>로봇이 아닙니다</title>
<style>
  body { font-family: system-ui, sans-serif; background:#0f1923; color:#ece8e1; display:flex;
    flex-direction:column; align-items:center; justify-content:center; min-height:100vh; margin:0; gap:1rem; padding:1rem; }
  h1 { font-size:1.15rem; font-weight:600; margin:0; }
  p { color:#9a9a9a; font-size:.9rem; margin:0; text-align:center; max-width:26rem; line-height:1.45; }
  #status { min-height:1.2rem; font-size:.9rem; text-align:center; max-width:26rem; white-space:pre-wrap; }
  .ok { color:#7cf5a0; }
  .err { color:#fd4553; }
  #captcha { min-height:78px; display:flex; justify-content:center; }
</style>
</head>
<body>
  <h1>로봇이 아닙니다</h1>
  <p>아래 체크만 완료하세요.<br/>2차 인증이 없는 계정이면 여기서 연동이 끝납니다.<br/>2FA가 있으면 Discord로 돌아가 코드를 입력합니다.</p>
  <div id="captcha"></div>
  <div id="status">준비 중…</div>
<script>
const state = %s;
let sitekey = '';
let rqdata = '';
let challengeVersion = 0;
let widgetId = null;
const statusEl = document.getElementById('status');

function setStatus(msg, cls) {
  statusEl.textContent = msg || '';
  statusEl.className = cls || '';
}

function bindRqdata() {
  try {
    if (typeof hcaptcha === 'undefined' || !hcaptcha.setData) return;
    if (widgetId != null) hcaptcha.setData(widgetId, { rqdata: rqdata });
    else hcaptcha.setData({ rqdata: rqdata });
  } catch (e) {}
}

function loadScript(src) {
  return new Promise((resolve, reject) => {
    const s = document.createElement('script');
    s.src = src;
    s.async = true;
    s.onload = () => resolve();
    s.onerror = () => reject(new Error('hCaptcha script blocked or failed to load'));
    document.head.appendChild(s);
  });
}

function renderWidget(resetStatus) {
  const el = document.getElementById('captcha');
  el.innerHTML = '';
  widgetId = hcaptcha.render(el, {
    sitekey: sitekey,
    size: 'normal',
    theme: 'dark',
    callback: onSolved,
    'expired-callback': () => setStatus('만료되었습니다. 다시 체크하세요.', 'err'),
    'error-callback': (err) => setStatus('캡차 오류 (' + (err || '?') + '). 새로고침 후 다시 시도하세요.', 'err'),
  });
  // Enterprise rqdata must be bound after render, before the user clicks.
  bindRqdata();
  setTimeout(bindRqdata, 50);
  if (resetStatus !== false) {
    setStatus('host=' + location.hostname + '\n「로봇이 아닙니다」를 체크하세요.', '');
  }
}

async function boot() {
  const host = (location.hostname || '').toLowerCase();
  if (host !== 'authenticate.riotgames.com') {
    setStatus('잘못된 호스트에서 열렸습니다: ' + location.host + '\nDiscord의 「캡차 창 다시 열기」를 누르세요.', 'err');
    return;
  }
  setStatus('세션 준비 중… (host=' + host + ')', '');
  let data;
  try {
    const res = await fetch('/api/auth/captcha/challenge?state=' + encodeURIComponent(state), {
      cache: 'no-store',
    });
    data = await res.json().catch(() => ({}));
    if (!res.ok || data.ok === false) {
      setStatus((data && data.error) || ('세션 준비 실패 (HTTP ' + res.status + ')'), 'err');
      return;
    }
  } catch (e) {
    setStatus('세션 준비 네트워크 오류: ' + e, 'err');
    return;
  }
  sitekey = data.sitekey || '';
  rqdata = data.rqdata || '';
  challengeVersion = Number(data.version || 0);
  if (!sitekey || !rqdata) {
    setStatus('Riot 캡차 데이터가 없습니다. Discord에서 /auth 를 다시 실행하세요.', 'err');
    return;
  }

  setStatus('체크박스 로딩 중…', '');
  try {
    await Promise.race([
      loadScript('https://js.hcaptcha.com/1/api.js?render=explicit&hl=ko'),
      new Promise((_, reject) => setTimeout(() => reject(new Error('hCaptcha 로딩 시간 초과 (15s)')), 15000)),
    ]);
  } catch (e) {
    setStatus(String(e.message || e) + '\n광고 차단을 끄거나 다른 브라우저로 다시 시도하세요.', 'err');
    return;
  }
  if (typeof hcaptcha === 'undefined') {
    setStatus('hCaptcha가 로드되지 않았습니다.', 'err');
    return;
  }
  // Bind rqdata globally before first render (enterprise).
  bindRqdata();
  renderWidget();
}

async function onSolved(token) {
  setStatus('확인 중…', '');
  try {
    const res = await fetch('/api/auth/captcha', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ state: state, token: token, version: challengeVersion }),
    });
    const data = await res.json().catch(() => ({}));
    if (data && data.retry) {
      sitekey = data.sitekey || sitekey;
      rqdata = data.rqdata || rqdata;
      challengeVersion = Number(data.version || challengeVersion);
      setStatus('캡차 토큰이 거절되어 새 체크가 필요합니다.\n다시 「로봇이 아닙니다」를 체크하세요.\n(' + (data.error || 'retry') + ')', 'err');
      try { if (widgetId != null) hcaptcha.reset(widgetId); } catch (e) {}
      bindRqdata();
      renderWidget(false);
      return;
    }
    if (!res.ok || data.ok === false) {
      const errMsg = (data && data.error) || '실패했습니다. Discord 메시지를 확인하세요.';
      if (String(errMsg).includes('invalid riot') || String(errMsg).includes('auth_failure')) {
        setStatus('Riot이 로그인을 거절했습니다.\n비밀번호가 확실하면 캡차 토큰 문제일 수 있습니다.\nDiscord에서 /auth 를 다시 실행하세요.', 'err');
      } else {
        setStatus(errMsg, 'err');
      }
      try { if (widgetId != null) hcaptcha.reset(widgetId); } catch (e) {}
      return;
    }
    if (data.mfa) {
      setStatus('완료. Discord로 돌아가 인증 코드를 입력하세요.', 'ok');
    } else {
      setStatus('연동 완료. Discord를 확인하세요.', 'ok');
    }
    setTimeout(() => { try { window.close(); } catch (e) {} }, 800);
  } catch (e) {
    setStatus('네트워크 오류: ' + e, 'err');
  }
}

boot();
</script>
</body>
</html>
`
