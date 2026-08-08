package authweb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/netutil"
	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

const (
	defaultPendingTTL     = 15 * time.Minute
	defaultQRPollInterval = 2 * time.Second
)

var (
	// ErrServerClosed is returned when an auth operation is canceled by server shutdown.
	ErrServerClosed = errors.New("auth server is shutting down")
	// ErrQROwner means a Discord user tried to cancel another user's QR login.
	ErrQROwner = errors.New("only the login owner can cancel this qr session")
	// ErrMFAOwner means a Discord user tried to control another user's MFA continuation.
	ErrMFAOwner = errors.New("only the login owner can continue this mfa session")
	// ErrMFAExpired covers missing, expired, and already-consumed MFA continuations.
	ErrMFAExpired = errors.New("unknown or expired mfa session")
)

// RiotRedirectURI is the OAuth redirect Riot returns to after login (riot-client).
// Must be served at http://localhost/redirect (port 80) on the browser machine.
const RiotRedirectURI = "http://localhost/redirect"

// Store persists auth pending state and Riot accounts.
type Store interface {
	PutAuthPending(state, discordUserID string, expiresAt time.Time) error
	TakeAuthPending(state string) (discordUserID string, ok bool, err error)
	TakeAuthPendingForOwner(state, discordUserID string) (deleted bool, err error)
	UpsertRiotAccount(a store.Account) error
}

// RiotClient talks to Riot auth / name APIs.
type RiotClient interface {
	GetEntitlements(ctx context.Context, accessToken string) (string, error)
	GetUserInfo(ctx context.Context, accessToken string) (string, error)
	GetPlayerNames(ctx context.Context, accessToken, entitlementsToken, shard string, puuids []string) ([]riot.PlayerName, error)
	ResolveValorantRegion(ctx context.Context, accessToken, idToken, fallback string) (region, shard string, err error)
}

// QRAuthClient drives the Riot Mobile QR login.
type QRAuthClient interface {
	StartQRSession(ctx context.Context) (*riot.QRSession, error)
	PollQRSession(ctx context.Context, sess *riot.QRSession) (loginToken string, err error)
	ExchangeLoginToken(ctx context.Context, loginToken string) (riot.QRTokens, error)
}

// PasswordAuthClient drives Discord modal username/password login with browser captcha.
type PasswordAuthClient interface {
	BeginCaptcha(ctx context.Context, username, password string, browser riot.CaptchaBrowserSession) (riot.CaptchaChallenge, error)
	CompleteCaptcha(ctx context.Context, sessionID, captchaToken string, browser riot.CaptchaBrowserSession) (riot.PasswordTokens, *riot.MFAChallenge, error)
	CancelCaptcha(sessionID string)
	SubmitMFA(ctx context.Context, challenge *riot.MFAChallenge, code string) (riot.PasswordTokens, error)
}

// Boxer encrypts session material at rest.
type Boxer interface {
	Encrypt(plain []byte) ([]byte, error)
}

// LinkedNotifier is called after a Riot account is linked (e.g. Discord DM).
type LinkedNotifier func(discordUserID, displayName string)

// Deps configures the auth web server.
type Deps struct {
	AuthBaseURL    string
	PendingTTL     time.Duration
	Store          Store
	Riot           RiotClient
	QRAuth         QRAuthClient
	PasswordAuth   PasswordAuthClient
	QRPollInterval time.Duration
	Boxer          Boxer
	OnLinked       LinkedNotifier
	// CaptchaTLSPort is retained for the legacy local-widget test/fallback path.
	// The production Riot browser flow uses Riot's real DNS/TLS and no listener.
	CaptchaTLSPort     int
	CaptchaBrowserMode netutil.CaptchaBrowserMode
	CaptchaDisplay     string
}

type authOutcome struct {
	Done    bool   `json:"done"`
	OK      bool   `json:"ok"`
	Display string `json:"display,omitempty"`
	Error   string `json:"error,omitempty"`
}

type mfaPending struct {
	discordUserID string
	challenge     *riot.MFAChallenge
	flow          *mfaFlow
	expiresAt     time.Time
}

type mfaFlow struct {
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	submitMu sync.Mutex
}

type captchaBrowserCloseFailure struct {
	controller      captchaBrowserController
	err             error
	possiblyRunning bool
	reaperAttempts  int
}

type serverMutex struct {
	sync.Mutex
	// Test-only lock-entry seam. A non-nil hook must return with the supplied
	// mutex locked; nil delegates directly to sync.Mutex.Lock.
	lockForTest func(*sync.Mutex)
}

func (m *serverMutex) Lock() {
	if hook := m.lockForTest; hook != nil {
		hook(&m.Mutex)
		return
	}
	m.Mutex.Lock()
}

// Server serves login redirect + Riot callback catcher.
type Server struct {
	authBaseURL         string
	remoteCaptchaOrigin string
	remoteCaptchaHost   string
	captchaBrowserMode  netutil.CaptchaBrowserMode
	captchaDisplay      string
	pendingTTL          time.Duration
	store               Store
	riot                RiotClient
	qrAuth              QRAuthClient
	passwordAuth        PasswordAuthClient
	qrPollInterval      time.Duration
	boxer               Boxer
	onLinked            LinkedNotifier
	mux                 *http.ServeMux
	captchaMux          *http.ServeMux

	mu                         serverMutex
	closed                     bool
	lifecycleCtx               context.Context
	lifecycleCancel            context.CancelFunc
	lifecycleWG                sync.WaitGroup
	shutdownOnce               sync.Once
	shutdownDone               chan struct{}
	shutdownErr                error
	outcomes                   map[string]authOutcome
	qrSessions                 map[string]qrPending
	mfaPending                 map[string]mfaPending
	passwordPending            map[string]passwordPending
	passwordOutcomes           map[string]passwordOutcome
	passwordReady              map[string]chan struct{}
	captchaCloseFailures       map[*passwordFlow]captchaBrowserCloseFailure
	captchaReaperRunning       bool
	captchaTLSConfiguredPort   int
	captchaTLSPort             int
	captchaTLSServer           *http.Server
	captchaTLSListener         net.Listener
	captchaTLSServeErr         error
	captchaTLSDone             chan struct{}
	launchCaptchaBrowser       func(string) (captchaBrowserController, error)
	launchRemoteCaptchaBrowser func(string, string) (captchaBrowserController, error)
	remoteCaptchaRandom        io.Reader
	remoteCaptchaNow           func() time.Time
	remoteCaptchaAfter         func(time.Duration) <-chan time.Time
	// Test-only synchronization seam for the cancellation claim critical section.
	beforePasswordWaitCancellationClaim func()
	// Test-only synchronization seam for the retained-browser reaper exit handoff.
	beforeCaptchaReaperIdleExit func()
	// Test-only synchronization seam after the final bounded reaper round.
	beforeCaptchaReaperMaxExit func()
	// Test-only synchronization seam after Shutdown publishes the closed boundary.
	afterShutdownClosedForTest func()
	// Test-only synchronization seam after lifecycle enrollment is complete.
	afterLifecycleAdmissionForTest func()
	// Test-only synchronization seams immediately around Shutdown's lifecycle join.
	beforeLifecycleDrainForTest func()
	afterLifecycleDrainForTest  func()
}

// New builds an auth web Server.
func New(d Deps) *Server {
	mode := d.CaptchaBrowserMode
	if mode == "" {
		mode = netutil.CaptchaBrowserLocal
	}
	display := d.CaptchaDisplay
	if display == "" {
		display = ":99"
	}
	ttl := d.PendingTTL
	if ttl <= 0 {
		ttl = defaultPendingTTL
	}
	poll := d.QRPollInterval
	if poll <= 0 {
		poll = defaultQRPollInterval
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	authBaseURL := strings.TrimRight(d.AuthBaseURL, "/")
	var remoteCaptchaOrigin, remoteCaptchaHost string
	if mode == netutil.CaptchaBrowserRemote {
		if canonicalOrigin, err := netutil.CanonicalRemoteCaptchaOrigin(d.AuthBaseURL); err == nil {
			authBaseURL = canonicalOrigin
			remoteCaptchaOrigin = canonicalOrigin
			if parsed, parseErr := url.Parse(canonicalOrigin); parseErr == nil {
				remoteCaptchaHost = parsed.Host
			}
		}
	}
	s := &Server{
		authBaseURL:                authBaseURL,
		remoteCaptchaOrigin:        remoteCaptchaOrigin,
		remoteCaptchaHost:          remoteCaptchaHost,
		captchaBrowserMode:         mode,
		captchaDisplay:             display,
		pendingTTL:                 ttl,
		store:                      d.Store,
		riot:                       d.Riot,
		qrAuth:                     d.QRAuth,
		passwordAuth:               d.PasswordAuth,
		qrPollInterval:             poll,
		boxer:                      d.Boxer,
		onLinked:                   d.OnLinked,
		mux:                        http.NewServeMux(),
		captchaMux:                 http.NewServeMux(),
		outcomes:                   make(map[string]authOutcome),
		qrSessions:                 make(map[string]qrPending),
		mfaPending:                 make(map[string]mfaPending),
		passwordPending:            make(map[string]passwordPending),
		passwordOutcomes:           make(map[string]passwordOutcome),
		passwordReady:              make(map[string]chan struct{}),
		captchaCloseFailures:       make(map[*passwordFlow]captchaBrowserCloseFailure),
		lifecycleCtx:               lifecycleCtx,
		lifecycleCancel:            lifecycleCancel,
		shutdownDone:               make(chan struct{}),
		captchaTLSConfiguredPort:   d.CaptchaTLSPort,
		launchCaptchaBrowser:       launchSystemChrome,
		launchRemoteCaptchaBrowser: launchSystemChromeOnDisplay,
		remoteCaptchaRandom:        rand.Reader,
		remoteCaptchaNow:           time.Now,
		remoteCaptchaAfter:         time.After,
	}
	if mode == netutil.CaptchaBrowserRemote {
		s.launchCaptchaBrowser = func(widgetURL string) (captchaBrowserController, error) {
			return s.launchRemoteCaptchaBrowser(widgetURL, display)
		}
		s.registerRemoteCaptchaHTTPRoutes()
	}
	if mode == netutil.CaptchaBrowserDisabled {
		s.launchCaptchaBrowser = disabledCaptchaBrowserLauncher
	}
	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("GET /redirect", s.handleRedirectCatcher)
	s.mux.HandleFunc("GET /catcher-ping", s.handleCatcherPing)
	s.mux.HandleFunc("POST /api/auth/callback", s.handleCallback)
	s.mux.HandleFunc("OPTIONS /api/auth/callback", s.handleCallbackCORS)
	s.mux.HandleFunc("GET /api/auth/wait", s.handleWait)
	s.mux.HandleFunc("GET /install-catcher.sh", s.handleInstallCatcher)
	s.captchaMux.HandleFunc("GET /captcha/widget", s.handleCaptchaWidgetPage)
	s.captchaMux.HandleFunc("GET /api/auth/captcha/challenge", s.handleCaptchaChallenge)
	s.captchaMux.HandleFunc("POST /api/auth/captcha", s.handleCaptchaSubmit)
	s.captchaMux.HandleFunc("OPTIONS /api/auth/captcha", s.handleCaptchaSubmit)
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	return s
}

func disabledCaptchaBrowserLauncher(string) (captchaBrowserController, error) {
	return nil, errors.New("password CAPTCHA is disabled (CAPTCHA_BROWSER_MODE=disabled); use Riot Mobile QR or set CAPTCHA_BROWSER_MODE=local or remote")
}

// Handler returns the HTTP handler (AUTH_PORT).
func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) captchaHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || !remoteAddrIsLoopback(r.RemoteAddr) {
			http.Error(w, "captcha transport forbidden", http.StatusForbidden)
			return
		}
		s.captchaMux.ServeHTTP(w, r)
	})
}

func remoteAddrIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.Trim(strings.TrimSpace(remoteAddr), "[]")
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleCatcherPing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}

// BeginAuth creates a pending auth state and returns the Discord login button URL.
func (s *Server) BeginAuth(discordUserID string) (loginURL, state string, err error) {
	_, lifecycleDone, err := s.beginLifecycleOperation(context.Background())
	if err != nil {
		return "", "", err
	}
	defer lifecycleDone()
	state, err = newState()
	if err != nil {
		return "", "", err
	}
	expiresAt := time.Now().Add(s.pendingTTL)
	if err := s.store.PutAuthPending(state, discordUserID, expiresAt); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_, _, rollbackErr := s.store.TakeAuthPending(state)
		if rollbackErr != nil {
			return "", "", errors.Join(ErrServerClosed, fmt.Errorf("rollback pending auth: %w", rollbackErr))
		}
		return "", "", ErrServerClosed
	}
	s.outcomes[state] = authOutcome{Done: false}
	s.mu.Unlock()
	loginURL = s.authBaseURL + "/login?state=" + url.QueryEscape(state)
	return loginURL, state, nil
}

type qrPending struct {
	session       *riot.QRSession
	discordUserID string
}

// BeginQRAuth starts a Riot Mobile QR login and returns the URL the user scans.
// The caller then blocks on WaitQRLogin with the returned state.
func (s *Server) BeginQRAuth(ctx context.Context, discordUserID string) (loginURL, state string, err error) {
	opCtx, done, err := s.beginLifecycleOperation(ctx)
	if err != nil {
		return "", "", err
	}
	defer done()
	if s.qrAuth == nil {
		return "", "", fmt.Errorf("qr auth not configured")
	}
	sess, err := s.qrAuth.StartQRSession(opCtx)
	if err != nil {
		return "", "", err
	}
	state, err = newState()
	if err != nil {
		return "", "", err
	}
	if err := s.store.PutAuthPending(state, discordUserID, time.Now().Add(s.pendingTTL)); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", "", ErrServerClosed
	}
	s.qrSessions[state] = qrPending{session: sess, discordUserID: discordUserID}
	s.mu.Unlock()
	return sess.LoginURL, state, nil
}

// CancelQRAuth removes a QR flow only for the Discord user that created it.
// Unknown states are already canceled and therefore succeed idempotently.
func (s *Server) CancelQRAuth(state, discordUserID string) error {
	s.mu.Lock()
	pending, ok := s.qrSessions[state]
	if ok && pending.discordUserID != discordUserID {
		s.mu.Unlock()
		return ErrQROwner
	}
	s.mu.Unlock()

	_, err := s.store.TakeAuthPendingForOwner(state, discordUserID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	current, stillLive := s.qrSessions[state]
	if ok && stillLive && current.session == pending.session && current.discordUserID == pending.discordUserID {
		delete(s.qrSessions, state)
	}
	s.mu.Unlock()
	return nil
}

// WaitQRLogin polls Riot until the QR code is approved, then links the account.
// It returns once the account is stored, ctx expires, or the QR session dies.
func (s *Server) WaitQRLogin(ctx context.Context, state string) (displayName string, err error) {
	opCtx, done, err := s.beginLifecycleOperation(ctx)
	if err != nil {
		return "", err
	}
	defer done()
	s.mu.Lock()
	pending, ok := s.qrSessions[state]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown or expired QR session")
	}
	sess := pending.session
	defer func() {
		s.mu.Lock()
		delete(s.qrSessions, state)
		s.mu.Unlock()
	}()

	ticker := time.NewTicker(s.qrPollInterval)
	defer ticker.Stop()

	for {
		loginToken, perr := s.qrAuth.PollQRSession(opCtx, sess)
		switch {
		case perr == nil:
			tokens, xerr := s.qrAuth.ExchangeLoginToken(opCtx, loginToken)
			if xerr != nil {
				if s.isClosed() {
					return "", ErrServerClosed
				}
				return "", xerr
			}
			return s.completeQRLogin(opCtx, state, tokens)
		case errors.Is(perr, riot.ErrQRPending):
		default:
			if s.isClosed() {
				return "", ErrServerClosed
			}
			return "", perr
		}

		select {
		case <-opCtx.Done():
			if s.isClosed() {
				return "", ErrServerClosed
			}
			return "", opCtx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) completeQRLogin(ctx context.Context, state string, tokens riot.QRTokens) (string, error) {
	discordUserID, ok, err := s.store.TakeAuthPending(state)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("unknown or expired state")
	}
	// Persisted QR logins hand back an ssid cookie, which CookieReauth can
	// refresh for daily store checks; otherwise fall back to the raw token.
	session := tokens.SessionCookie
	if session == "" {
		session = "access_token=" + tokens.AccessToken
	}
	return s.linkAccount(ctx, discordUserID, tokens.AccessToken, tokens.IDToken, session, "")
}

// ValidatePasswordMFA checks an MFA continuation in memory before Discord opens
// its modal. It intentionally performs no Riot or persistence calls.
func (s *Server) ValidatePasswordMFA(mfaState, discordUserID string) (hint string, err error) {
	mfaState = strings.TrimSpace(mfaState)
	discordUserID = strings.TrimSpace(discordUserID)
	s.mu.Lock()
	pending, ok := s.mfaPending[mfaState]
	live := ok && pending.flow != nil && pending.flow.ctx.Err() == nil && time.Now().Before(pending.expiresAt)
	s.mu.Unlock()
	if !live {
		return "", ErrMFAExpired
	}
	if pending.discordUserID != discordUserID {
		return "", ErrMFAOwner
	}
	return formatMFAHint(pending.challenge), nil
}

// CancelPasswordMFA atomically detaches an owner-bound continuation, cancels
// its flow, and joins any in-flight submission without holding Server.mu. A
// missing or already-consumed state is an idempotent success.
func (s *Server) CancelPasswordMFA(mfaState, discordUserID string) error {
	mfaState = strings.TrimSpace(mfaState)
	discordUserID = strings.TrimSpace(discordUserID)
	s.mu.Lock()
	pending, ok := s.mfaPending[mfaState]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	if pending.discordUserID != discordUserID {
		s.mu.Unlock()
		return ErrMFAOwner
	}
	delete(s.mfaPending, mfaState)
	s.mu.Unlock()
	if pending.flow != nil {
		pending.flow.cancel()
		pending.flow.wg.Wait()
	}
	return nil
}

// CompletePasswordMFA finishes a password login after the Discord MFA modal.
func (s *Server) CompletePasswordMFA(ctx context.Context, mfaState, discordUserID, code string) (displayName string, err error) {
	if s.passwordAuth == nil {
		return "", fmt.Errorf("password auth not configured")
	}
	opCtx, lifecycleDone, err := s.beginLifecycleOperation(ctx)
	if err != nil {
		return "", err
	}
	defer lifecycleDone()
	mfaState = strings.TrimSpace(mfaState)
	discordUserID = strings.TrimSpace(discordUserID)
	pending, done, ok := s.beginMFAOperation(mfaState)
	if !ok {
		if s.isClosed() {
			return "", ErrServerClosed
		}
		return "", ErrMFAExpired
	}
	defer done()
	if pending.discordUserID != discordUserID {
		return "", ErrMFAOwner
	}
	pending.flow.submitMu.Lock()
	defer pending.flow.submitMu.Unlock()

	pending, ok = s.liveMFAState(mfaState, pending.flow)
	if !ok {
		if s.isClosed() {
			return "", ErrServerClosed
		}
		return "", ErrMFAExpired
	}
	if pending.discordUserID != discordUserID {
		return "", ErrMFAOwner
	}
	requestCtx, requestCancel := context.WithCancel(opCtx)
	stop := context.AfterFunc(pending.flow.ctx, requestCancel)
	tokens, err := s.passwordAuth.SubmitMFA(requestCtx, pending.challenge, code)
	stop()
	requestCancel()
	pending, live := s.liveMFAState(mfaState, pending.flow)
	if !live {
		if s.isClosed() {
			return "", ErrServerClosed
		}
		return "", ErrMFAExpired
	}
	if err != nil {
		if errors.Is(err, riot.ErrPasswordInvalidCode) {
			// Riot has not consumed the challenge; the owner may retry a fresh code.
			return "", err
		}
		// Transport and all other Riot failures are terminal because the remote
		// challenge's retry safety is unknown.
		if !s.consumeMFAState(mfaState, pending.flow) {
			return "", ErrMFAExpired
		}
		pending.flow.cancel()
		return "", err
	}

	// Atomically claim this one-time MFA state before linking. Any concurrent
	// modal submission will observe the missing state after submitMu unlocks.
	if !s.consumeMFAState(mfaState, pending.flow) {
		return "", ErrMFAExpired
	}
	pending.flow.cancel()
	displayName, err = s.completePasswordTokens(opCtx, pending.discordUserID, tokens)
	if err != nil && s.isClosed() {
		return "", ErrServerClosed
	}
	return displayName, err
}

func (s *Server) consumeMFAState(mfaState string, flow *mfaFlow) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.mfaPending[mfaState]
	if !ok || current.flow != flow || flow == nil || flow.ctx.Err() != nil || !time.Now().Before(current.expiresAt) {
		return false
	}
	delete(s.mfaPending, mfaState)
	return true
}

func (s *Server) beginMFAOperation(mfaState string) (mfaPending, func(), bool) {
	s.mu.Lock()
	pending, ok := s.mfaPending[mfaState]
	if s.closed || !ok || pending.flow == nil || pending.flow.ctx.Err() != nil || time.Now().After(pending.expiresAt) {
		s.mu.Unlock()
		return mfaPending{}, nil, false
	}
	pending.flow.wg.Add(1)
	s.mu.Unlock()
	return pending, pending.flow.wg.Done, true
}

func (s *Server) liveMFAState(mfaState string, flow *mfaFlow) (mfaPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.mfaPending[mfaState]
	if !ok || pending.flow != flow || flow.ctx.Err() != nil || time.Now().After(pending.expiresAt) {
		return mfaPending{}, false
	}
	return pending, true
}

func (s *Server) cleanupMFAState(mfaState string) {
	s.mu.Lock()
	pending, ok := s.mfaPending[mfaState]
	delete(s.mfaPending, mfaState)
	s.mu.Unlock()
	if ok && pending.flow != nil {
		pending.flow.cancel()
		pending.flow.wg.Wait()
	}
}

func (s *Server) expireMFAState(mfaState string) {
	defer s.lifecycleWG.Done()
	for {
		s.mu.Lock()
		pending, ok := s.mfaPending[mfaState]
		s.mu.Unlock()
		if !ok {
			return
		}
		wait := time.Until(pending.expiresAt)
		if wait <= 0 {
			s.cleanupMFAState(mfaState)
			return
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-pending.flow.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (s *Server) completePasswordTokens(ctx context.Context, discordUserID string, tokens riot.PasswordTokens) (string, error) {
	session := tokens.SessionCookie
	if session == "" {
		session = "access_token=" + tokens.AccessToken
	}
	return s.linkAccount(ctx, discordUserID, tokens.AccessToken, tokens.IDToken, session, "")
}

// WaitBrowserLogin blocks until the browser OAuth callback completes for state.
func (s *Server) WaitBrowserLogin(ctx context.Context, state string) (displayName string, err error) {
	opCtx, lifecycleDone, err := s.beginLifecycleOperation(ctx)
	if err != nil {
		return "", err
	}
	defer lifecycleDone()
	ticker := time.NewTicker(s.qrPollInterval)
	defer ticker.Stop()
	for {
		if o, ok := s.getOutcome(state); ok && o.Done {
			if !o.OK {
				if o.Error != "" {
					return "", fmt.Errorf("%s", o.Error)
				}
				return "", errors.New("browser login failed")
			}
			return o.Display, nil
		}
		select {
		case <-opCtx.Done():
			if s.isClosed() {
				return "", ErrServerClosed
			}
			return "", opCtx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) setOutcome(state string, o authOutcome) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.outcomes[state] = o
	return true
}

func (s *Server) getOutcome(state string) (authOutcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.outcomes[state]
	return o, ok
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

// CompleteFromRedirectURL links a Riot account from an OAuth redirect URL
// captured when Riot sends the browser to http://localhost/redirect (this bot).
func (s *Server) CompleteFromRedirectURL(ctx context.Context, state, redirectURL, regionFallback string) (displayName string, err error) {
	opCtx, lifecycleDone, err := s.beginLifecycleOperation(ctx)
	if err != nil {
		return "", err
	}
	defer lifecycleDone()
	displayName, err = s.completeFromRedirectURL(opCtx, state, redirectURL, regionFallback)
	if err != nil && s.isClosed() {
		return "", ErrServerClosed
	}
	return displayName, err
}

func (s *Server) completeFromRedirectURL(ctx context.Context, state, redirectURL, regionFallback string) (displayName string, err error) {
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
	prepared, err := s.prepareAccountLink(ctx, discordUserID, accessToken, idToken, "access_token="+accessToken, regionFallback)
	if err != nil {
		return "", err
	}
	// Claim the irreversible commit before Server.closed can advance. Shutdown
	// joins the enrolled operation, so a commit that wins this boundary may
	// finish without holding Server.mu across store or notifier I/O.
	s.mu.Lock()
	if s.closed || ctx.Err() != nil {
		s.mu.Unlock()
		return "", ErrServerClosed
	}
	s.mu.Unlock()
	if err := s.commitAccountLink(prepared); err != nil {
		return "", err
	}
	return prepared.display, nil
}

type preparedAccountLink struct {
	account store.Account
	display string
}

// prepareAccountLink resolves and encrypts an account without making the
// irreversible store/notifier commit. Callers with lifecycle state can claim
// their commit boundary after this context-cancelable work completes.
func (s *Server) prepareAccountLink(ctx context.Context, discordUserID, accessToken, idToken, session, regionFallback string) (preparedAccountLink, error) {
	if err := ctx.Err(); err != nil {
		return preparedAccountLink{}, err
	}
	entitlements, err := s.riot.GetEntitlements(ctx, accessToken)
	if err != nil {
		return preparedAccountLink{}, fmt.Errorf("entitlements: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return preparedAccountLink{}, err
	}

	puuid, err := s.riot.GetUserInfo(ctx, accessToken)
	if err != nil {
		return preparedAccountLink{}, fmt.Errorf("userinfo: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return preparedAccountLink{}, err
	}

	region, shard, err := s.riot.ResolveValorantRegion(ctx, accessToken, idToken, regionFallback)
	if err != nil {
		return preparedAccountLink{}, fmt.Errorf("region: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return preparedAccountLink{}, err
	}

	gameName, tagLine := "", ""
	if names, nerr := s.riot.GetPlayerNames(ctx, accessToken, entitlements, shard, []string{puuid}); nerr == nil && len(names) > 0 {
		gameName = names[0].GameName
		tagLine = names[0].TagLine
	} else if idToken != "" {
		if gn, tl, ierr := riot.GameNameFromIDToken(idToken); ierr == nil {
			gameName, tagLine = gn, tl
		} else if nerr != nil {
			return preparedAccountLink{}, fmt.Errorf("player names: %w", nerr)
		}
	} else if nerr != nil {
		return preparedAccountLink{}, fmt.Errorf("player names: %w", nerr)
	}
	if err := ctx.Err(); err != nil {
		return preparedAccountLink{}, err
	}

	cipherText, err := s.boxer.Encrypt([]byte(session))
	if err != nil {
		return preparedAccountLink{}, err
	}
	if err := ctx.Err(); err != nil {
		return preparedAccountLink{}, err
	}

	return preparedAccountLink{
		account: store.Account{
			DiscordUserID:     discordUserID,
			PUUID:             puuid,
			GameName:          gameName,
			TagLine:           tagLine,
			Region:            region,
			Shard:             shard,
			CookiesCiphertext: cipherText,
		},
		display: gameName + "#" + tagLine,
	}, nil
}

func (s *Server) commitAccountLink(prepared preparedAccountLink) error {
	if err := s.store.UpsertRiotAccount(prepared.account); err != nil {
		return err
	}
	if s.onLinked != nil {
		s.onLinked(prepared.account.DiscordUserID, prepared.display)
	}
	return nil
}

// linkAccount resolves the Riot identity behind accessToken and stores it for
// discordUserID. session is the material encrypted at rest for later reauth.
func (s *Server) linkAccount(ctx context.Context, discordUserID, accessToken, idToken, session, regionFallback string) (string, error) {
	prepared, err := s.prepareAccountLink(ctx, discordUserID, accessToken, idToken, session, regionFallback)
	if err != nil {
		return "", err
	}
	if err := s.commitAccountLink(prepared); err != nil {
		return "", err
	}
	return prepared.display, nil
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

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		http.Error(w, "missing state — Discord에서 /auth 를 다시 실행하세요.", http.StatusBadRequest)
		return
	}
	riotURL := riotAuthorizeURL(state)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, loginPageHTML,
		html.EscapeString(s.authBaseURL),
		s.authBaseURL,
		state,
		riotURL,
	)
}

func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if o, ok := s.getOutcome(state); ok && o.Done {
		_ = json.NewEncoder(w).Encode(o)
		return
	}
	_ = json.NewEncoder(w).Encode(authOutcome{Done: false})
}

func (s *Server) handleInstallCatcher(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline; filename=\"install-catcher.sh\"")
	_, _ = fmt.Fprintf(w, installCatcherScript, s.authBaseURL)
}

func (s *Server) handleRedirectCatcher(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, redirectCatcherHTML, s.authBaseURL+"/api/auth/callback")
}

func (s *Server) handleCallbackCORS(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	opCtx, lifecycleDone, lifecycleErr := s.beginLifecycleOperation(r.Context())
	if lifecycleErr != nil {
		http.Error(w, lifecycleErr.Error(), http.StatusServiceUnavailable)
		return
	}
	defer lifecycleDone()
	r = r.WithContext(opCtx)
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

	display, err := s.completeFromRedirectURL(opCtx, state, redirectURL, region)
	if err != nil {
		if s.isClosed() {
			err = ErrServerClosed
		}
		log.Printf("auth callback error: %v", err)
		if !s.setOutcome(state, authOutcome{Done: true, OK: false, Error: err.Error()}) {
			err = ErrServerClosed
		}
		msg := err.Error()
		code := http.StatusBadRequest
		if errors.Is(err, ErrServerClosed) {
			code = http.StatusServiceUnavailable
		}
		if strings.Contains(msg, "entitlements") || strings.Contains(msg, "userinfo") || strings.Contains(msg, "player names") {
			code = http.StatusBadGateway
		}
		http.Error(w, msg, code)
		return
	}

	if !s.setOutcome(state, authOutcome{Done: true, OK: true, Display: display}) {
		http.Error(w, ErrServerClosed.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, successHTML, html.EscapeString(display))
}

const redirectCatcherHTML = `<!DOCTYPE html>
<html lang="ko">
<head><meta charset="utf-8"><title>연동 처리 중…</title></head>
<body>
<p id="msg">Discord 계정에 연결하는 중…</p>
<script>
(function () {
  var callback = %q;
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
      document.open(); document.write(text); document.close();
    });
  }).catch(function (err) {
    msg.textContent = "저장 실패: " + err.message;
  });
})();
</script>
</body>
</html>
`

// loginPageHTML args: display authBase (%s), then authBase, state, riotURL (%q ×3).
const loginPageHTML = `<!DOCTYPE html>
<html lang="ko">
<head>
<meta charset="utf-8">
<title>Riot 계정 연동</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 36rem; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
  .btn { display: inline-block; background: #fd4553; color: #fff; padding: .7rem 1.2rem; border-radius: 8px; text-decoration: none; font-weight: 600; border: 0; cursor: pointer; font-size: 1rem; }
  .muted { color: #666; font-size: .9rem; }
  #status { margin-top: 1rem; font-weight: 600; }
</style>
</head>
<body>
<h1>Riot 계정 연동</h1>
<p id="status">준비 중…</p>
<p><button class="btn" type="button" id="loginBtn" style="display:none">Riot으로 로그인</button></p>
<p class="muted">브라우저에서 로그인·2차 인증을 마치면 자동으로 Discord에 연동됩니다.</p>
<p class="muted">서버: %s</p>
<script>
(function () {
  var authBase = %q;
  var state = %q;
  var riotURL = %q;
  var status = document.getElementById("status");
  var btn = document.getElementById("loginBtn");
  var popup = null;
  var started = false;

  function openRiot() {
    if (started) return;
    started = true;
    status.textContent = "Riot 로그인 창에서 로그인하세요. 2차 인증이 있으면 그 창에서 완료하세요…";
    btn.style.display = "none";
    popup = window.open(riotURL, "riot_auth", "width=520,height=720");
    if (!popup) {
      status.textContent = "팝업이 차단되었습니다. 아래 버튼으로 다시 시도하세요.";
      btn.style.display = "inline-block";
      started = false;
    }
  }

  btn.addEventListener("click", openRiot);

  function pingLocalhost() {
    return fetch("http://127.0.0.1/catcher-ping", { mode: "cors", cache: "no-store" })
      .then(function (r) { return r.ok; })
      .catch(function () { return false; });
  }

  function pollWait() {
    fetch(authBase + "/api/auth/wait?state=" + encodeURIComponent(state), { cache: "no-store" })
      .then(function (r) { return r.json(); })
      .then(function (o) {
        if (o && o.done) {
          if (o.ok) {
            status.textContent = "연동 완료: " + (o.display || "");
            document.body.insertAdjacentHTML("beforeend",
              "<p>이 창을 닫고 Discord에서 <code>/shop</code> 을 사용하세요.</p>");
            if (popup && !popup.closed) popup.close();
            return;
          }
          status.textContent = "연동 실패: " + (o.error || "unknown");
          return;
        }
        setTimeout(pollWait, 1500);
      })
      .catch(function () { setTimeout(pollWait, 2000); });
  }

  pingLocalhost().then(function (ok) {
    if (ok) {
      status.textContent = "봇이 localhost를 소유 중입니다. Riot 로그인을 엽니다…";
      openRiot();
      return;
    }
    status.textContent = "이 PC에서 봇이 localhost:80을 열지 못했습니다. 봇을 이 컴퓨터에서 실행한 뒤 다시 시도하거나 Discord에서 Riot Mobile QR을 사용하세요.";
    btn.style.display = "inline-block";
  });

  pollWait();
})();
</script>
</body>
</html>
`

const indexHTML = `<!DOCTYPE html>
<html lang="ko">
<head><meta charset="utf-8"><title>Valorant Bot Auth</title></head>
<body style="font-family:system-ui;max-width:36rem;margin:2rem auto;padding:0 1rem">
<h1>Valorant Bot</h1>
<p>Discord에서 <code>/auth</code> 를 실행하고, 표시된 QR 코드를 <strong>Riot Mobile</strong> 앱으로 스캔해 로그인을 승인하세요.</p>
<p>이 페이지는 브라우저 로그인 예비 경로입니다. 평소에는 필요하지 않습니다.</p>
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
