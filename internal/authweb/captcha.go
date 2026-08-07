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
	discordUserID  string
	username       string
	password       string
	sessionID      string
	siteKey        string
	rqData         string
	challengeVer   uint64
	challengeWait  chan struct{}
	browserUA      string
	browserCookies []*http.Cookie
	flow           *passwordFlow
	expiresAt      time.Time
}

type passwordFlow struct {
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	launchMu sync.Mutex
	submitMu sync.Mutex
	browser  captchaBrowserController
	// Lifecycle fields below are protected by Server.mu.
	sealed           bool
	commitClaimed    bool
	cleanupRequested bool
}

type passwordOutcome struct {
	done     bool
	display  string
	mfaState string
	mfaHint  string
	err      error
}

type passwordStateCleanup struct {
	pending passwordPending
	flow    *passwordFlow
	claimed bool
}

type passwordWaitDecision struct {
	outcome passwordOutcome
	ready   <-chan struct{}
	cleanup passwordStateCleanup
	done    bool
}

var errPasswordFlowClosed = errors.New("captcha session closed; run /auth again")

// BeginPasswordLogin stores credentials and prepares a button-launched
// bot-host Chrome flow.
// captchaURL is intentionally empty: Discord uses a custom-ID button to ask the
// bot process to launch Chrome, never a localhost/public link on the user device.
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

	return "", state, nil
}

// LaunchPasswordCaptcha validates the Discord owner and asks the bot host to
// wait for loopback TLS before opening a fresh Chrome window. It never exposes
// loopback URLs to Discord users.
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
	if err := s.waitCaptchaTLS(3 * time.Second); err != nil {
		return err
	}
	return s.ensureCaptchaLaunched(state)
}

// WaitPasswordLogin blocks until the captcha page completes (success, MFA, or terminal error).
// Captcha retries (new widget challenge) do not finish this wait.
func (s *Server) WaitPasswordLogin(ctx context.Context, state string) (displayName, mfaState, mfaHint string, err error) {
	ticker := time.NewTicker(s.qrPollInterval)
	defer ticker.Stop()
	for {
		decision := s.claimPasswordWait(state, nil)
		if decision.done {
			s.finishPasswordStateCleanup(decision.cleanup)
			out := decision.outcome
			return out.display, out.mfaState, out.mfaHint, out.err
		}
		select {
		case <-ctx.Done():
			decision = s.claimPasswordWait(state, ctx.Err())
			s.finishPasswordStateCleanup(decision.cleanup)
			out := decision.outcome
			return out.display, out.mfaState, out.mfaHint, out.err
		case <-decision.ready:
		case <-ticker.C:
		}
	}
}

// claimPasswordWait is the single short linearization point between a
// completed password outcome and waiter cancellation. A completed outcome is
// captured before the password state is detached; otherwise cancelErr claims
// the state so no later publisher can revive it.
func (s *Server) claimPasswordWait(state string, cancelErr error) passwordWaitDecision {
	s.mu.Lock()
	defer s.mu.Unlock()

	if out, ok := s.passwordOutcomes[state]; ok && out.done {
		return passwordWaitDecision{
			outcome: out,
			cleanup: s.claimPasswordStateCleanupLocked(state),
			done:    true,
		}
	}
	if cancelErr != nil {
		if hook := s.beforePasswordWaitCancellationClaim; hook != nil {
			hook()
		}
		return passwordWaitDecision{
			outcome: passwordOutcome{done: true, err: cancelErr},
			cleanup: s.claimPasswordStateCleanupLocked(state),
			done:    true,
		}
	}
	return passwordWaitDecision{ready: s.passwordReady[state]}
}

func (s *Server) claimPasswordFinalization(state string, flow *passwordFlow) error {
	if flow == nil {
		return errPasswordFlowClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.passwordPending[state]
	current := s.passwordOutcomes[state]
	if !ok || pending.flow != flow || flow.sealed || current.done || flow.ctx.Err() != nil ||
		time.Now().After(pending.expiresAt) {
		return errPasswordFlowClosed
	}
	flow.sealed = true
	scrubPasswordCredentials(&pending)
	s.passwordPending[state] = pending
	return nil
}

func (s *Server) publishPasswordOutcome(state string, flow *passwordFlow, out passwordOutcome) (passwordOutcome, error) {
	out.done = true
	s.mu.Lock()
	pending, ok := s.passwordPending[state]
	current := s.passwordOutcomes[state]
	if !ok || pending.flow != flow || !flow.sealed || flow.commitClaimed || current.done ||
		flow.ctx.Err() != nil || time.Now().After(pending.expiresAt) {
		s.mu.Unlock()
		return passwordOutcome{}, errPasswordFlowClosed
	}
	s.passwordOutcomes[state] = out
	if ready := s.passwordReady[state]; ready != nil {
		close(ready)
		delete(s.passwordReady, state)
	}
	s.mu.Unlock()
	return out, nil
}

func (s *Server) setPasswordOutcome(state string, flow *passwordFlow, out passwordOutcome) (passwordOutcome, error) {
	if err := s.claimPasswordFinalization(state, flow); err != nil {
		return passwordOutcome{}, err
	}
	closeErr := s.closeOwnedCaptchaBrowser(flow)
	if captchaBrowserMayBeRunning(closeErr) {
		out = passwordOutcome{err: fmt.Errorf("captcha Chrome could not be closed: %w", closeErr)}
	}
	published, publishErr := s.publishPasswordOutcome(state, flow, out)
	if publishErr != nil && captchaBrowserMayBeRunning(closeErr) {
		return passwordOutcome{}, errors.Join(publishErr, out.err)
	}
	return published, publishErr
}

func (s *Server) claimPasswordAccountCommit(state string, flow *passwordFlow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.passwordPending[state]
	current := s.passwordOutcomes[state]
	if !ok || pending.flow != flow || !flow.sealed || flow.commitClaimed || current.done ||
		flow.ctx.Err() != nil || time.Now().After(pending.expiresAt) {
		return errPasswordFlowClosed
	}
	// This short transition is the irreversible persistence linearization
	// point. Cleanup that acquires Server.mu first cancels and removes the
	// flow; cleanup after this point records a request and lets commit finish.
	flow.commitClaimed = true
	return nil
}

func (s *Server) publishCommittedPasswordOutcome(state string, flow *passwordFlow, out passwordOutcome) (passwordOutcome, error) {
	out.done = true
	s.mu.Lock()
	pending, ok := s.passwordPending[state]
	current := s.passwordOutcomes[state]
	if !ok || pending.flow != flow || !flow.sealed || !flow.commitClaimed || current.done {
		s.mu.Unlock()
		return passwordOutcome{}, errPasswordFlowClosed
	}
	flow.commitClaimed = false
	cleanupRequested := flow.cleanupRequested
	s.passwordOutcomes[state] = out
	if ready := s.passwordReady[state]; ready != nil {
		close(ready)
		delete(s.passwordReady, state)
	}
	s.mu.Unlock()
	if cleanupRequested {
		go s.cleanupPasswordState(state)
	}
	return out, nil
}

func passwordSession(tokens riot.PasswordTokens) string {
	if tokens.SessionCookie != "" {
		return tokens.SessionCookie
	}
	return "access_token=" + tokens.AccessToken
}

func (s *Server) finishPasswordAccount(ctx context.Context, state string, pending passwordPending, tokens riot.PasswordTokens) (passwordOutcome, error) {
	flow := pending.flow
	if err := s.claimPasswordFinalization(state, flow); err != nil {
		return passwordOutcome{}, err
	}
	closeErr := s.closeOwnedCaptchaBrowser(flow)
	if captchaBrowserMayBeRunning(closeErr) {
		out := passwordOutcome{err: fmt.Errorf("captcha Chrome could not be closed: %w", closeErr)}
		published, publishErr := s.publishPasswordOutcome(state, flow, out)
		if publishErr != nil {
			return passwordOutcome{}, errors.Join(publishErr, out.err)
		}
		return published, nil
	}

	prepared, err := s.prepareAccountLink(ctx, pending.discordUserID, tokens.AccessToken, tokens.IDToken, passwordSession(tokens), "")
	if err != nil {
		return s.publishPasswordOutcome(state, flow, passwordOutcome{err: err})
	}
	if err := s.claimPasswordAccountCommit(state, flow); err != nil {
		return passwordOutcome{}, err
	}
	commitErr := s.commitAccountLink(prepared)
	out := passwordOutcome{display: prepared.display, err: commitErr}
	if commitErr != nil {
		out.display = ""
	}
	return s.publishCommittedPasswordOutcome(state, flow, out)
}

func (s *Server) finishPasswordMFA(state string, pending passwordPending, mfaState string, challenge *riot.MFAChallenge, hint string) (passwordOutcome, error) {
	flow := pending.flow
	if err := s.claimPasswordFinalization(state, flow); err != nil {
		return passwordOutcome{}, err
	}
	closeErr := s.closeOwnedCaptchaBrowser(flow)
	if captchaBrowserMayBeRunning(closeErr) {
		out := passwordOutcome{err: fmt.Errorf("captcha Chrome could not be closed: %w", closeErr)}
		published, publishErr := s.publishPasswordOutcome(state, flow, out)
		if publishErr != nil {
			return passwordOutcome{}, errors.Join(publishErr, out.err)
		}
		return published, nil
	}

	out := passwordOutcome{done: true, mfaState: mfaState, mfaHint: hint}
	s.mu.Lock()
	current, ok := s.passwordPending[state]
	published := s.passwordOutcomes[state]
	if !ok || current.flow != flow || !flow.sealed || flow.commitClaimed || published.done ||
		flow.ctx.Err() != nil || time.Now().After(current.expiresAt) {
		s.mu.Unlock()
		return passwordOutcome{}, errPasswordFlowClosed
	}
	mfaCtx, mfaCancel := context.WithCancel(context.Background())
	s.mfaPending[mfaState] = mfaPending{
		discordUserID: pending.discordUserID,
		challenge:     challenge,
		flow:          &mfaFlow{ctx: mfaCtx, cancel: mfaCancel},
		expiresAt:     time.Now().Add(s.pendingTTL),
	}
	s.passwordOutcomes[state] = out
	if ready := s.passwordReady[state]; ready != nil {
		close(ready)
		delete(s.passwordReady, state)
	}
	s.mu.Unlock()
	go s.expireMFAState(mfaState)
	return out, nil
}

func passwordOutcomeFailure(out passwordOutcome, publishErr error) error {
	if publishErr != nil {
		return publishErr
	}
	return out.err
}

func (s *Server) cleanupPasswordState(state string) {
	s.mu.Lock()
	cleanup := s.claimPasswordStateCleanupLocked(state)
	s.mu.Unlock()
	s.finishPasswordStateCleanup(cleanup)
}

// claimPasswordStateCleanupLocked detaches a password state while Server.mu is
// held. Slow controller and operation cleanup is returned to the caller.
func (s *Server) claimPasswordStateCleanupLocked(state string) passwordStateCleanup {
	pending, ok := s.passwordPending[state]
	if !ok {
		return passwordStateCleanup{}
	}
	flow := pending.flow
	if flow != nil && flow.commitClaimed {
		// The commit claim won. Do not cancel the context underneath an
		// irreversible store/notifier operation; request cleanup and return
		// immediately. Publication schedules the actual cleanup afterward.
		flow.cleanupRequested = true
		return passwordStateCleanup{}
	}
	if flow != nil {
		flow.sealed = true
	}
	scrubPasswordCredentials(&pending)
	delete(s.passwordPending, state)
	delete(s.passwordOutcomes, state)
	delete(s.passwordReady, state)
	return passwordStateCleanup{pending: pending, flow: flow, claimed: true}
}

func (s *Server) finishPasswordStateCleanup(cleanup passwordStateCleanup) {
	if !cleanup.claimed {
		return
	}
	if cleanup.flow != nil {
		cleanup.flow.cancel()
		if closeErr := s.closeOwnedCaptchaBrowser(cleanup.flow); closeErr != nil {
			log.Printf("captcha Chrome cleanup remains incomplete: %v", closeErr)
		}
		cleanup.flow.wg.Wait()
	}
	if cleanup.pending.sessionID != "" && s.passwordAuth != nil {
		s.passwordAuth.CancelCaptcha(cleanup.pending.sessionID)
	}
}

func scrubPasswordCredentials(pending *passwordPending) {
	if pending == nil {
		return
	}
	pending.username = ""
	pending.password = ""
}

func (s *Server) beginPasswordOperation(state string) (passwordPending, func(), bool) {
	s.mu.Lock()
	pending, ok := s.passwordPending[state]
	out := s.passwordOutcomes[state]
	if !ok || pending.flow == nil || pending.flow.sealed || out.done ||
		pending.flow.ctx.Err() != nil || time.Now().After(pending.expiresAt) {
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
	if !ok || pending.flow == nil || pending.flow.sealed || pending.flow.ctx.Err() != nil ||
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

func (s *Server) ensureCaptchaChallenge(ctx context.Context, state string, browser riot.CaptchaBrowserSession) (passwordPending, error) {
	browser.UserAgent = strings.TrimSpace(browser.UserAgent)
	if browser.UserAgent == "" {
		return passwordPending{}, fmt.Errorf("captcha browser user-agent missing")
	}
	for {
		s.mu.Lock()
		pending, ok := s.passwordPending[state]
		out := s.passwordOutcomes[state]
		if !ok || pending.flow == nil || pending.flow.sealed || out.done ||
			pending.flow.ctx.Err() != nil || time.Now().After(pending.expiresAt) {
			s.mu.Unlock()
			return passwordPending{}, fmt.Errorf("captcha session expired; run /auth again")
		}
		if pending.sessionID != "" && pending.siteKey != "" && pending.rqData != "" {
			if pending.browserUA != browser.UserAgent {
				s.mu.Unlock()
				return passwordPending{}, fmt.Errorf("%w: captcha browser identity changed", riot.ErrCaptchaSession)
			}
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
		ch, err := s.passwordAuth.BeginCaptcha(beginCtx, pending.username, pending.password, browser)
		cancel()

		s.mu.Lock()
		cur, stillPending := s.passwordPending[state]
		out = s.passwordOutcomes[state]
		stillPending = stillPending && cur.flow == pending.flow && !cur.flow.sealed && !out.done
		if stillPending && cur.challengeWait == wait {
			cur.challengeWait = nil
			if err == nil {
				cur.sessionID = ch.SessionID
				cur.siteKey = ch.SiteKey
				cur.rqData = ch.RQData
				cur.challengeVer++
				cur.browserUA = browser.UserAgent
				cur.browserCookies = cloneHTTPCookies(ch.BrowserCookies)
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
	out := s.passwordOutcomes[state]
	live := ok && pending.flow != nil && !pending.flow.sealed && !out.done && time.Now().Before(pending.expiresAt)
	s.mu.Unlock()
	if !live {
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
	if !captchaRiotHost(r.Host) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "captcha must use " + RiotCaptchaHost})
		return
	}
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

	pending, err := s.ensureCaptchaChallenge(ctx, state, captchaBrowserSession(r))
	if err != nil {
		log.Printf("captcha challenge failed: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeCaptchaBrowserCookies(w, pending.browserCookies)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"sitekey": pending.siteKey,
		"rqdata":  pending.rqData,
		"version": pending.challengeVer,
	})
}

func (s *Server) handleCaptchaSubmit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !captchaRiotHost(r.Host) || r.Header.Get("Origin") != "https://"+r.Host {
		http.Error(w, `{"ok":false,"error":"invalid captcha origin"}`, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "https://"+r.Host)
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
		writeCaptchaBrowserCookies(w, pending.browserCookies)
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

	tokens, challenge, err := s.passwordAuth.CompleteCaptcha(ctx, pending.sessionID, req.Token, captchaBrowserSession(r))
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
			out := s.passwordOutcomes[req.State]
			stillLive = stillLive && p.flow == pending.flow && !p.flow.sealed && !out.done && p.flow.ctx.Err() == nil &&
				p.sessionID == pending.sessionID && time.Now().Before(p.expiresAt)
			if !stillLive {
				s.mu.Unlock()
				http.Error(w, `{"ok":false,"error":"expired"}`, http.StatusBadRequest)
				return
			}
			p.siteKey = retry.SiteKey
			p.rqData = retry.RQData
			p.challengeVer++
			p.browserCookies = cloneHTTPCookies(retry.BrowserCookies)
			p.expiresAt = time.Now().Add(s.pendingTTL)
			s.passwordPending[req.State] = p
			s.mu.Unlock()
			writeCaptchaBrowserCookies(w, retry.BrowserCookies)
			log.Printf("captcha retry reason=%s", retry.Reason)
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
			s.passwordAuth.CancelCaptcha(pending.sessionID)
			writeCaptchaBrowserCookieClears(w, r.Cookies())
			s.mu.Lock()
			if p, ok := s.passwordPending[req.State]; ok && p.flow == pending.flow && !p.flow.sealed &&
				!s.passwordOutcomes[req.State].done && p.sessionID == pending.sessionID {
				p.sessionID = ""
				p.siteKey = ""
				p.rqData = ""
				p.browserUA = ""
				p.browserCookies = nil
				p.challengeVer++
				s.passwordPending[req.State] = p
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     false,
				"reload": true,
				"error":  "captcha session expired; loading a fresh challenge",
			})
			return
		}
		log.Printf("captcha submit failed: %v", err)
		published, publishErr := s.setPasswordOutcome(req.State, pending.flow, passwordOutcome{err: err})
		outcomeErr := passwordOutcomeFailure(published, publishErr)
		if outcomeErr == nil {
			outcomeErr = errPasswordFlowClosed
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": outcomeErr.Error()})
		return
	}
	if challenge != nil {
		mfaState, err := newState()
		if err != nil {
			published, publishErr := s.setPasswordOutcome(req.State, pending.flow, passwordOutcome{err: err})
			outcomeErr := passwordOutcomeFailure(published, publishErr)
			if outcomeErr == nil {
				outcomeErr = errPasswordFlowClosed
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": outcomeErr.Error()})
			return
		}
		hint := formatMFAHint(challenge)
		published, publishErr := s.finishPasswordMFA(req.State, pending, mfaState, challenge, hint)
		if outcomeErr := passwordOutcomeFailure(published, publishErr); outcomeErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": outcomeErr.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "mfa": true})
		return
	}

	published, publishErr := s.finishPasswordAccount(ctx, req.State, pending, tokens)
	if outcomeErr := passwordOutcomeFailure(published, publishErr); outcomeErr != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": outcomeErr.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "mfa": false, "display": published.display})
}

func captchaRiotHost(rawHost string) bool {
	host := strings.ToLower(strings.TrimSpace(rawHost))
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host == RiotCaptchaHost
}

func captchaBrowserSession(r *http.Request) riot.CaptchaBrowserSession {
	cookies := make(map[string]string)
	for _, cookie := range r.Cookies() {
		cookies[cookie.Name] = cookie.Value
	}
	return riot.CaptchaBrowserSession{
		UserAgent: strings.TrimSpace(r.UserAgent()),
		Cookies:   cookies,
	}
}

func writeCaptchaBrowserCookies(w http.ResponseWriter, cookies []*http.Cookie) {
	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}
		clone := *cookie
		http.SetCookie(w, &clone)
	}
}

func writeCaptchaBrowserCookieClears(w http.ResponseWriter, cookies []*http.Cookie) {
	seen := make(map[string]struct{})
	for _, cookie := range cookies {
		if cookie == nil || cookie.Name == "" {
			continue
		}
		if _, ok := seen[cookie.Name]; ok {
			continue
		}
		seen[cookie.Name] = struct{}{}
		for _, domain := range []string{"", "riotgames.com"} {
			http.SetCookie(w, &http.Cookie{
				Name:     cookie.Name,
				Value:    "",
				Domain:   domain,
				Path:     "/",
				MaxAge:   -1,
				Secure:   true,
				HttpOnly: true,
			})
		}
	}
}

func cloneHTTPCookies(cookies []*http.Cookie) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}
		clone := *cookie
		out = append(out, &clone)
	}
	return out
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
  #verify { appearance:none; border:0; border-radius:.25rem; padding:.8rem 1.25rem; font-size:1rem;
    font-weight:700; color:#fff; background:#d1363a; cursor:pointer; }
  #verify:disabled { opacity:.55; cursor:wait; }
</style>
</head>
<body>
  <h1>로봇이 아닙니다</h1>
  <p>아래 버튼을 눌러 사람 확인을 완료하세요.<br/>2차 인증이 없는 계정이면 여기서 연동이 끝납니다.<br/>2FA가 있으면 Discord로 돌아가 코드를 입력합니다.</p>
  <div id="captcha"></div>
  <button id="verify" type="button" disabled>사람 확인 시작</button>
  <div id="status">준비 중…</div>
<script>
const state = %s;
let sitekey = '';
let rqdata = '';
let challengeVersion = 0;
let widgetId = null;
const statusEl = document.getElementById('status');
const verifyEl = document.getElementById('verify');

function setStatus(msg, cls) {
  statusEl.textContent = msg || '';
  statusEl.className = cls || '';
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
  if (widgetId != null) {
    try { hcaptcha.remove(widgetId); } catch (e) {}
  }
  el.innerHTML = '';
  widgetId = hcaptcha.render(el, {
    sitekey: sitekey,
    size: 'invisible',
    theme: 'dark',
    callback: onSolved,
    'expired-callback': () => captchaReady('만료되었습니다. 다시 시도하세요.', 'err'),
    'chalexpired-callback': () => captchaReady('확인 시간이 만료되었습니다. 다시 시도하세요.', 'err'),
    'close-callback': () => captchaReady('사람 확인 창이 닫혔습니다. 다시 시도하세요.', 'err'),
    'error-callback': (err) => captchaReady('캡차 오류 (' + (err || '?') + '). 다시 시도하세요.', 'err'),
  });
  verifyEl.disabled = false;
  if (resetStatus !== false) {
    setStatus('host=' + location.hostname + '\n「사람 확인 시작」을 누르세요.', '');
  }
}

function captchaReady(message, cls) {
  verifyEl.disabled = false;
  setStatus(message, cls);
}

function beginVerify() {
  if (widgetId == null || !rqdata || verifyEl.disabled) return;
  verifyEl.disabled = true;
  setStatus('사람 확인 중…', '');
  try {
    const execution = hcaptcha.execute(widgetId, {rqdata: rqdata});
    if (execution && typeof execution.catch === 'function') {
      execution.catch((err) => captchaReady('캡차 실행 오류: ' + err, 'err'));
    }
  } catch (e) {
    captchaReady('캡차 실행 오류: ' + e, 'err');
  }
}

async function refreshCaptchaChallenge() {
  setStatus('Riot 응답을 확인하지 못했습니다. 현재 세션을 다시 확인하는 중…', 'err');
  try {
    const res = await fetch('/api/auth/captcha/challenge?state=' + encodeURIComponent(state), {
      cache: 'no-store',
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok || data.ok === false) {
      setStatus((data && data.error) || '세션 확인에 실패했습니다. Discord를 확인하고 /auth 를 다시 실행하세요.', 'err');
      return;
    }
    sitekey = data.sitekey || '';
    rqdata = data.rqdata || '';
    challengeVersion = Number(data.version || 0);
    if (!sitekey || !rqdata || !challengeVersion) {
      setStatus('현재 Riot 캡차 데이터를 복구하지 못했습니다. /auth 를 다시 실행하세요.', 'err');
      return;
    }
    renderWidget(false);
    setStatus('응답이 유실되어 새 사람 확인이 필요합니다.\n「사람 확인 시작」을 다시 누르세요.', 'err');
  } catch (refreshError) {
    setStatus('세션 복구 네트워크 오류: ' + refreshError + '\n연결이 복구되면 이 페이지를 새로고침하세요.', 'err');
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

  setStatus('사람 확인 로딩 중…', '');
  try {
    await Promise.race([
      loadScript('https://hcaptcha.com/1/api.js?render=explicit&hl=ko'),
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
		if (data && data.reload) {
			await refreshCaptchaChallenge();
			return;
		}
		if (data && data.retry) {
      sitekey = data.sitekey || sitekey;
      rqdata = data.rqdata || rqdata;
      challengeVersion = Number(data.version || challengeVersion);
      setStatus('캡차 토큰이 거절되어 새 확인이 필요합니다.\n다시 「사람 확인 시작」을 누르세요.\n(' + (data.error || 'retry') + ')', 'err');
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
      verifyEl.disabled = false;
      return;
    }
    if (data.mfa) {
      setStatus('완료. Discord로 돌아가 인증 코드를 입력하세요.', 'ok');
    } else {
      setStatus('연동 완료. Discord를 확인하세요.', 'ok');
    }
    setTimeout(() => { try { window.close(); } catch (e) {} }, 800);
  } catch (e) {
    await refreshCaptchaChallenge();
  }
}

verifyEl.addEventListener('click', beginVerify);
boot();
</script>
</body>
</html>
`
