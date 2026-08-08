package riot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrPasswordInvalid     = errors.New("invalid riot username or password")
	ErrPasswordRateLimit   = errors.New("riot login rate limited; try again later")
	ErrPasswordCaptcha     = errors.New("riot captcha rejected; solve the new captcha")
	ErrPasswordInvalidCode = errors.New("invalid multifactor code")
	ErrCaptchaSession      = errors.New("unknown or expired captcha session")
)

// mfaSchemaRejectedError is returned only when Riot explicitly rejects an MFA
// payload before it can consume the OTP. It intentionally carries no response
// body because those bodies are untrusted and may contain sensitive material.
type mfaSchemaRejectedError struct{}

const mfaNonConsumingSchemaRejection = "multifactor request schema rejected before otp processing"
const maxPasswordResponseBody = 1 << 20

func (*mfaSchemaRejectedError) Error() string {
	return "riot rejected the MFA payload schema"
}

// CaptchaRetryError means Riot rejected the token but issued a fresh challenge.
// The browser page should re-render the widget with SiteKey/RQData.
type CaptchaRetryError struct {
	SiteKey        string
	RQData         string
	Reason         string
	BrowserCookies []*http.Cookie
}

func (e *CaptchaRetryError) Error() string {
	if e == nil {
		return "riot captcha retry required"
	}
	if e.Reason != "" {
		return "riot captcha retry required: " + e.Reason
	}
	return "riot captcha retry required"
}

func (e *CaptchaRetryError) Is(target error) bool {
	return target == ErrPasswordCaptcha
}

const captchaSessionTTL = 10 * time.Minute

// PasswordTokens are session tokens from username/password (or MFA) login.
type PasswordTokens struct {
	AccessToken   string
	IDToken       string
	SessionCookie string
}

// CaptchaBrowserSession is the HTTP identity of the Chrome window that solves
// hCaptcha. Riot's challenge cookies and User-Agent must remain identical when
// the resulting token is submitted.
type CaptchaBrowserSession struct {
	UserAgent string
	Cookies   map[string]string
}

// CaptchaChallenge is the hCaptcha widget data for browser solving.
type CaptchaChallenge struct {
	SessionID      string
	SiteKey        string
	RQData         string
	BrowserCookies []*http.Cookie
}

// MFAChallenge means Riot needs a second factor before handing out tokens.
type MFAChallenge struct {
	Email     string
	Method    string
	Methods   []string
	cookies   map[string]string
	sdkSID    string
	userAgent string
	// authenticate is true when MFA must be submitted to authenticate.riotgames.com.
	authenticate bool
}

type captchaSession struct {
	username       string
	password       string
	cookies        map[string]string
	browserCookies map[string]*http.Cookie
	sdkSID         string
	userAgent      string
	siteKey        string
	rqData         string
	expires        time.Time
}

// PasswordClient performs Riot ID/password auth via authenticate.riotgames.com
// (browser hCaptcha, then optional Discord MFA modal).
type PasswordClient struct {
	HTTPClient          *http.Client
	AuthBaseURL         string
	AuthenticateBaseURL string
	UserAgent           string
	SDKVersion          string

	metaOnce sync.Once
	metaMu   sync.RWMutex
	mu       sync.Mutex
	sessions map[string]*captchaSession
}

// NewPasswordClient builds a client against Riot production endpoints.
func NewPasswordClient(httpClient *http.Client) *PasswordClient {
	if httpClient == nil {
		// Avoid hanging captcha page/API on stalled DNS or TLS (DefaultClient has no timeout).
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &PasswordClient{
		HTTPClient:          httpClient,
		AuthBaseURL:         "https://auth.riotgames.com",
		AuthenticateBaseURL: defaultAuthenticateBaseURL,
		sessions:            make(map[string]*captchaSession),
	}
}

type captchaWebSetCookie struct {
	sourceHost string
	cookie     *http.Cookie
}

// BrowserAuthorizeURL returns Riot's official browser login entry point. The
// nonce is bound to the Discord password state so a fresh Chrome profile can
// be identified without exposing credentials in the URL.
func (c *PasswordClient) BrowserAuthorizeURL(nonce string) (string, error) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return "", errors.New("missing browser login nonce")
	}
	authBase, err := url.Parse(c.authBase())
	if err != nil {
		return "", fmt.Errorf("parse auth base: %w", err)
	}
	authBase.Path = strings.TrimRight(authBase.Path, "/") + "/authorize"
	query := authBase.Query()
	query.Set("client_id", "riot-client")
	query.Set("nonce", nonce)
	query.Set("redirect_uri", qrRedirectURI)
	query.Set("response_type", "token id_token")
	query.Set("scope", qrScope)
	authBase.RawQuery = query.Encode()
	return authBase.String(), nil
}

// AdoptBrowserLogin converts an authenticate.riotgames.com response produced
// by the owned Chrome session into the existing token/MFA continuation. Only
// cookies that the browser would send back to the login endpoint are retained.
func (c *PasswordClient) AdoptBrowserLogin(ctx context.Context, raw []byte, browserCookies []*http.Cookie, userAgent string) (PasswordTokens, *MFAChallenge, error) {
	if len(raw) == 0 || len(raw) > maxPasswordResponseBody {
		return PasswordTokens{}, nil, errors.New("invalid browser login response")
	}
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return PasswordTokens{}, nil, fmt.Errorf("%w: missing browser user-agent", ErrCaptchaSession)
	}
	target, err := url.Parse(c.authenticateBase() + "/api/v1/login")
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("parse authenticate base: %w", err)
	}
	cookies := make(map[string]string)
	for _, cookie := range browserCookies {
		if cookie == nil || captchaCookieDeletes(cookie) {
			continue
		}
		if _, allowed := captchaBrowserCookieAllowlist[cookie.Name]; !allowed {
			continue
		}
		if strings.EqualFold(target.Scheme, "https") && !cookie.Secure {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
		host := strings.ToLower(target.Hostname())
		if domain != "" && domain != host && !strings.HasSuffix(host, "."+domain) {
			continue
		}
		if !captchaCookiePathMatches(target.Path, cookie.Path) {
			continue
		}
		cookies[cookie.Name] = cookie.Value
	}
	return c.parseAuthenticateLogin(ctx, raw, cookies, true, "", userAgent)
}

// beginCaptchaWebSession follows Riot's browser authorization entry point so
// the CAPTCHA rqdata belongs to the same web session as the later credential
// PUT. A cookie jar is used only for this credential-free redirect chain; the
// resulting authenticate.riotgames.com cookies are then copied into the
// short-lived in-memory CAPTCHA session.
func (c *PasswordClient) beginCaptchaWebSession(ctx context.Context, userAgent string) (map[string]string, []*http.Cookie, error) {
	authBase, err := url.Parse(c.authBase())
	if err != nil {
		return nil, nil, fmt.Errorf("parse auth base: %w", err)
	}
	authenticateBase, err := url.Parse(c.authenticateBase())
	if err != nil {
		return nil, nil, fmt.Errorf("parse authenticate base: %w", err)
	}
	authURLString, err := c.BrowserAuthorizeURL(randomHex(16))
	if err != nil {
		return nil, nil, err
	}
	authURL, err := url.Parse(authURLString)
	if err != nil {
		return nil, nil, fmt.Errorf("parse browser authorize URL: %w", err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create captcha cookie jar: %w", err)
	}
	baseClient := c.HTTPClient
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	webClient := *baseClient
	webClient.Jar = jar
	allowed := map[string]string{
		strings.ToLower(authBase.Host):         strings.ToLower(authBase.Scheme),
		strings.ToLower(authenticateBase.Host): strings.ToLower(authenticateBase.Scheme),
	}
	observed := make([]captchaWebSetCookie, 0, 8)
	capture := func(resp *http.Response) {
		if resp == nil || resp.Request == nil || resp.Request.URL == nil {
			return
		}
		for _, cookie := range resp.Cookies() {
			if cookie == nil {
				continue
			}
			clone := *cookie
			observed = append(observed, captchaWebSetCookie{sourceHost: strings.ToLower(resp.Request.URL.Hostname()), cookie: &clone})
		}
	}
	webClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		capture(req.Response)
		if len(via) >= 10 {
			return errors.New("too many Riot web authorization redirects")
		}
		scheme, ok := allowed[strings.ToLower(req.URL.Host)]
		if !ok || !strings.EqualFold(req.URL.Scheme, scheme) {
			return fmt.Errorf("Riot web authorization redirected to untrusted origin %s", req.URL.Redacted())
		}
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", userAgent)
	resp, err := webClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("Riot web authorization: %w", err)
	}
	capture(resp)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxPasswordResponseBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("Riot web authorization: %s", resp.Status)
	}
	if resp.Request == nil || resp.Request.URL == nil || !strings.EqualFold(resp.Request.URL.Host, authenticateBase.Host) {
		return nil, nil, errors.New("Riot web authorization did not reach authenticate.riotgames.com")
	}

	loginURL := *authenticateBase
	loginURL.Path = strings.TrimRight(loginURL.Path, "/") + "/api/v1/login"
	jarCookies := make(map[string]string)
	for _, cookie := range jar.Cookies(&loginURL) {
		if cookie == nil {
			continue
		}
		if _, allowed := captchaBrowserCookieAllowlist[cookie.Name]; allowed {
			jarCookies[cookie.Name] = cookie.Value
		}
	}
	browserCookies := captchaWebBrowserCookies(observed, authenticateBase.Hostname(), loginURL.Path, jarCookies, strings.EqualFold(authenticateBase.Scheme, "https"))
	cookies := make(map[string]string, len(browserCookies))
	for _, cookie := range browserCookies {
		cookies[cookie.Name] = cookie.Value
	}
	return cookies, browserCookies, nil
}

func captchaWebBrowserCookies(observed []captchaWebSetCookie, targetHost, requestPath string, canonical map[string]string, requireSecure bool) []*http.Cookie {
	active := make(map[string]*http.Cookie)
	targetHost = strings.ToLower(targetHost)
	for _, item := range observed {
		cookie := item.cookie
		if cookie == nil || (requireSecure && !cookie.Secure) {
			continue
		}
		if _, allowed := captchaBrowserCookieAllowlist[cookie.Name]; !allowed {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
		if domain == "" {
			if item.sourceHost != targetHost {
				continue
			}
		} else if targetHost != domain && !strings.HasSuffix(targetHost, "."+domain) {
			continue
		}
		if !captchaCookiePathMatches(requestPath, cookie.Path) {
			continue
		}
		if captchaCookieDeletes(cookie) {
			delete(active, cookie.Name)
			continue
		}
		clone := *cookie
		active[cookie.Name] = &clone
	}
	for name, cookie := range active {
		value, ok := canonical[name]
		if !ok {
			delete(active, name)
			continue
		}
		cookie.Value = value
	}
	return captchaBrowserCookieSlice(active)
}

// BeginCaptcha starts the Riot authenticate login and returns hCaptcha widget data.
// Credentials are held only in memory until login completes or the session expires.
func (c *PasswordClient) BeginCaptcha(ctx context.Context, username, password string, browser CaptchaBrowserSession) (CaptchaChallenge, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return CaptchaChallenge{}, ErrPasswordInvalid
	}
	userAgent := strings.TrimSpace(browser.UserAgent)
	if userAgent == "" {
		return CaptchaChallenge{}, fmt.Errorf("%w: missing browser user-agent", ErrCaptchaSession)
	}
	c.refreshClientMeta(ctx)

	cookies, webCookies, err := c.beginCaptchaWebSession(ctx, userAgent)
	if err != nil {
		return CaptchaChallenge{}, fmt.Errorf("captcha web session: %w", err)
	}
	resp, err := c.doAuthenticateWebJSON(ctx, http.MethodGet, c.authenticateBase()+"/api/v1/login", nil, cookies, userAgent)
	if err != nil {
		return CaptchaChallenge{}, fmt.Errorf("captcha begin: %w", err)
	}
	defer resp.Body.Close()
	raw, err := readPasswordResponse(resp.Body)
	if err != nil {
		return CaptchaChallenge{}, fmt.Errorf("captcha begin response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return CaptchaChallenge{}, fmt.Errorf("captcha begin: %s", resp.Status)
	}
	siteKey, rqData, ok := extractHCaptcha(raw)
	if !ok {
		var out struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &out)
		if out.Error != "" {
			return CaptchaChallenge{}, fmt.Errorf("captcha begin: %s", out.Error)
		}
		return CaptchaChallenge{}, errors.New("captcha begin: missing hcaptcha challenge")
	}
	setCookies := resp.Cookies()
	browserCookies := mergeCaptchaBrowserCookies(nil, webCookies)
	browserCookies = mergeCaptchaBrowserCookies(browserCookies, setCookies)
	browserChanges := append(append([]*http.Cookie(nil), webCookies...), setCookies...)
	browserCookieSync := captchaBrowserCookieSync(browserCookies, browserChanges, browser.Cookies, cookies, true)

	sessionID := randomHex(16)
	c.mu.Lock()
	if c.sessions == nil {
		c.sessions = make(map[string]*captchaSession)
	}
	c.purgeExpiredLocked()
	c.sessions[sessionID] = &captchaSession{
		username:       username,
		password:       password,
		cookies:        cookies,
		browserCookies: browserCookies,
		sdkSID:         "",
		userAgent:      userAgent,
		siteKey:        siteKey,
		rqData:         rqData,
		expires:        time.Now().Add(captchaSessionTTL),
	}
	c.mu.Unlock()

	return CaptchaChallenge{
		SessionID:      sessionID,
		SiteKey:        siteKey,
		RQData:         rqData,
		BrowserCookies: browserCookieSync,
	}, nil
}

// CompleteCaptcha submits the browser hCaptcha token with stored credentials.
// On MFA it returns a non-nil *MFAChallenge.
// On captcha rejection with a fresh challenge it returns *CaptchaRetryError and keeps the session.
func (c *PasswordClient) CompleteCaptcha(ctx context.Context, sessionID, captchaToken string, browser CaptchaBrowserSession) (PasswordTokens, *MFAChallenge, error) {
	captchaToken = strings.TrimSpace(captchaToken)
	if captchaToken == "" {
		return PasswordTokens{}, nil, ErrPasswordCaptcha
	}

	c.mu.Lock()
	sess, ok := c.sessions[sessionID]
	if ok && time.Now().After(sess.expires) {
		delete(c.sessions, sessionID)
		ok = false
	}
	if !ok || sess == nil {
		c.mu.Unlock()
		return PasswordTokens{}, nil, ErrCaptchaSession
	}
	if err := validateCaptchaBrowserSession(sess, browser); err != nil {
		c.mu.Unlock()
		return PasswordTokens{}, nil, err
	}
	// Copy credentials/cookies; keep session for captcha retry.
	username := sess.username
	password := sess.password
	cookies := captchaSessionCookies(sess.cookies)
	browserCookies := copyCaptchaBrowserCookies(sess.browserCookies)
	sdkSID := sess.sdkSID
	userAgent := sess.userAgent
	c.mu.Unlock()

	token := captchaToken
	if !strings.HasPrefix(strings.ToLower(token), "hcaptcha ") {
		token = "hcaptcha " + token
	}

	// Match working Riot Client / community clients (authenticate.riotgames.com).
	body := map[string]any{
		"type":     "auth",
		"language": "ko_KR",
		"remember": false,
		"riot_identity": map[string]any{
			"captcha":  token,
			"password": password,
			"username": username,
		},
	}
	resp, err := c.doAuthenticateWebJSON(ctx, http.MethodPut, c.authenticateBase()+"/api/v1/login", body, cookies, userAgent)
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("captcha complete: %w", err)
	}
	defer resp.Body.Close()
	raw, err := readPasswordResponse(resp.Body)
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("captcha complete response: %w", err)
	}
	setCookies := resp.Cookies()
	browserCookies = mergeCaptchaBrowserCookies(browserCookies, setCookies)
	browserCookieSync := captchaBrowserCookieSync(browserCookies, setCookies, browser.Cookies, cookies, false)

	// Persist cookies from the attempt back into the session for retries/MFA.
	c.updateSessionCookies(sessionID, cookies, browserCookies)

	if resp.StatusCode == http.StatusTooManyRequests {
		return PasswordTokens{}, nil, ErrPasswordRateLimit
	}

	reason := authenticateErrorReason(raw)
	siteKey, rqData, hasChallenge := extractHCaptcha(raw)
	var perr error
	if resp.StatusCode == http.StatusOK {
		var tokens PasswordTokens
		var mfa *MFAChallenge
		tokens, mfa, perr = c.parseAuthenticateLogin(ctx, raw, cookies, true, sdkSID, userAgent)
		if perr == nil {
			c.deleteSession(sessionID)
			return tokens, mfa, nil
		}
	} else {
		// A proxy or upstream error body may resemble a valid MFA/success
		// continuation. Classify only explicit semantic errors on non-OK
		// responses; never parse or exchange login tokens from them.
		perr = passwordAuthenticateStatusError(resp.Status, reason)
	}

	cookieNames := make([]string, 0, len(cookies))
	for name := range cookies {
		cookieNames = append(cookieNames, name)
	}
	log.Printf(
		"riot captcha complete status=%d reason=%s hasChallenge=%v errorID=%q cfRay=%q cookies=%v",
		resp.StatusCode,
		reason,
		hasChallenge,
		resp.Header.Get("X-RSO-Error-Id"),
		resp.Header.Get("CF-Ray"),
		cookieNames,
	)

	// Riot often returns auth_failure both for bad passwords AND bad captcha tokens.
	// When a fresh captcha blob is attached, prefer retry over blaming the password.
	if reason == "auth_failure" || reason == "invalid_credentials" || errors.Is(perr, ErrPasswordInvalid) {
		if hasChallenge {
			c.updateSessionChallenge(sessionID, siteKey, rqData, cookies)
			log.Printf("riot auth_failure with new captcha; retry challenge")
			return PasswordTokens{}, nil, &CaptchaRetryError{
				SiteKey:        siteKey,
				RQData:         rqData,
				Reason:         reason,
				BrowserCookies: browserCookieSync,
			}
		}
		c.deleteSession(sessionID)
		log.Printf("riot password rejected after captcha (%s)", reason)
		return PasswordTokens{}, nil, ErrPasswordInvalid
	}

	// Captcha / request rejected — keep session when Riot sends a new widget.
	if hasChallenge && (isCaptchaError(reason) || reason == "invalid_request" || errors.Is(perr, ErrPasswordCaptcha)) {
		c.updateSessionChallenge(sessionID, siteKey, rqData, cookies)
		log.Printf("riot captcha rejected (%s); issuing retry challenge", reason)
		return PasswordTokens{}, nil, &CaptchaRetryError{
			SiteKey:        siteKey,
			RQData:         rqData,
			Reason:         reason,
			BrowserCookies: browserCookieSync,
		}
	}

	// invalid_request without a captcha blob usually means the token/session was
	// rejected; surface it as a captcha problem (not a cryptic riot auth string).
	if reason == "invalid_request" || errors.Is(perr, ErrPasswordCaptcha) {
		c.deleteSession(sessionID)
		return PasswordTokens{}, nil, fmt.Errorf("%w: invalid_request (captcha/session rejected — run /auth and solve again)", ErrPasswordCaptcha)
	}

	if resp.StatusCode != http.StatusOK && perr != nil {
		return PasswordTokens{}, nil, perr
	}
	if errors.Is(perr, ErrPasswordRateLimit) || errors.Is(perr, ErrPasswordInvalidCode) {
		return PasswordTokens{}, nil, perr
	}
	return PasswordTokens{}, nil, perr
}

// CancelCaptcha immediately removes credentials and cookies retained for an
// abandoned browser captcha flow.
func (c *PasswordClient) CancelCaptcha(sessionID string) {
	c.deleteSession(strings.TrimSpace(sessionID))
}

// SubmitMFA continues after the user provides an email or authenticator OTP.
func (c *PasswordClient) SubmitMFA(ctx context.Context, challenge *MFAChallenge, code string) (PasswordTokens, error) {
	if challenge == nil || challenge.cookies == nil {
		return PasswordTokens{}, errors.New("missing mfa challenge")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return PasswordTokens{}, ErrPasswordInvalidCode
	}

	body := map[string]any{
		"type": "multifactor",
		"multifactor": map[string]any{
			"otp":            code,
			"rememberDevice": true,
		},
	}
	tokens, mfa, err := c.submitMFAPayload(ctx, challenge, body)
	var schemaErr *mfaSchemaRejectedError
	if errors.As(err, &schemaErr) {
		// Only a typed, non-consuming request-schema rejection may use the
		// compatibility payload. Never retry after a transport, token-exchange,
		// parsing, or other ambiguous result because the OTP may be consumed.
		fallback := map[string]any{
			"type":           "multifactor",
			"code":           code,
			"rememberDevice": true,
		}
		tokens, mfa, err = c.submitMFAPayload(ctx, challenge, fallback)
	}
	if err == nil && mfa == nil {
		return tokens, nil
	}
	if errors.Is(err, ErrPasswordInvalidCode) || mfa != nil || errors.Is(err, ErrPasswordInvalid) {
		return PasswordTokens{}, ErrPasswordInvalidCode
	}
	if err == nil {
		err = ErrPasswordInvalidCode
	}
	return PasswordTokens{}, err
}

func (c *PasswordClient) submitMFAPayload(ctx context.Context, challenge *MFAChallenge, body map[string]any) (PasswordTokens, *MFAChallenge, error) {
	if challenge.authenticate {
		return c.putAuthenticate(ctx, challenge.cookies, body, challenge.sdkSID, challenge.userAgent)
	}
	return c.putAuth(ctx, challenge.cookies, body, challenge.sdkSID, challenge.userAgent)
}

func (c *PasswordClient) putAuthenticate(ctx context.Context, cookies map[string]string, body map[string]any, sdkSID, userAgent string) (PasswordTokens, *MFAChallenge, error) {
	resp, err := c.doAuthenticateWebJSON(ctx, http.MethodPut, c.authenticateBase()+"/api/v1/login", body, cookies, userAgent)
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("authenticate put: %w", err)
	}
	defer resp.Body.Close()
	raw, err := readPasswordResponse(resp.Body)
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("authenticate put response: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return PasswordTokens{}, nil, ErrPasswordRateLimit
	}
	if isMFASchemaRejection(resp.StatusCode, body, raw) {
		return PasswordTokens{}, nil, &mfaSchemaRejectedError{}
	}
	// Outside the exact non-consuming schema rejection above, a non-OK status
	// is ambiguous: Riot may already have consumed the OTP. Never interpret a
	// proxy/server error body as MFA retry, success, or a login token.
	if resp.StatusCode != http.StatusOK {
		return PasswordTokens{}, nil, fmt.Errorf("authenticate put: %s", resp.Status)
	}
	tokens, mfa, perr := c.parseAuthenticateLogin(ctx, raw, cookies, true, sdkSID, userAgent)
	return tokens, mfa, perr
}

func (c *PasswordClient) parseAuthenticateLogin(ctx context.Context, raw []byte, cookies map[string]string, authenticateMFA bool, sdkSID, userAgent string) (PasswordTokens, *MFAChallenge, error) {
	var out struct {
		Type    string `json:"type"`
		Error   string `json:"error"`
		Success struct {
			LoginToken string `json:"login_token"`
		} `json:"success"`
		Multifactor struct {
			Email   string   `json:"email"`
			Method  string   `json:"method"`
			Methods []string `json:"methods"`
		} `json:"multifactor"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("authenticate decode: %w", err)
	}

	isMFA := out.Type == "multifactor" ||
		out.Multifactor.Method != "" ||
		out.Multifactor.Email != "" ||
		len(out.Multifactor.Methods) > 0
	if isMFA {
		method := out.Multifactor.Method
		if method == "" && len(out.Multifactor.Methods) > 0 {
			method = out.Multifactor.Methods[0]
		}
		ch := &MFAChallenge{
			Email:        out.Multifactor.Email,
			Method:       method,
			Methods:      append([]string(nil), out.Multifactor.Methods...),
			cookies:      copyCookies(cookies),
			sdkSID:       sdkSID,
			userAgent:    userAgent,
			authenticate: authenticateMFA,
		}
		if out.Error == "invalid_code" {
			return PasswordTokens{}, ch, ErrPasswordInvalidCode
		}
		return PasswordTokens{}, ch, nil
	}

	switch {
	case out.Error == "auth_failure", out.Error == "invalid_credentials":
		return PasswordTokens{}, nil, ErrPasswordInvalid
	case out.Error == "rate_limited":
		return PasswordTokens{}, nil, ErrPasswordRateLimit
	case out.Type == "success" && out.Success.LoginToken != "":
		tok, err := c.exchangeLoginToken(ctx, out.Success.LoginToken)
		return tok, nil, err
	case isCaptchaError(out.Error), out.Type == "error" && hasHCaptcha(raw):
		return PasswordTokens{}, nil, ErrPasswordCaptcha
	case out.Error != "":
		return PasswordTokens{}, nil, fmt.Errorf("riot auth: %s", out.Error)
	case out.Type == "auth" && hasHCaptcha(raw):
		return PasswordTokens{}, nil, ErrPasswordCaptcha
	default:
		return PasswordTokens{}, nil, fmt.Errorf("riot auth: unexpected response type=%q", out.Type)
	}
}

func (c *PasswordClient) putAuth(ctx context.Context, cookies map[string]string, body map[string]any, sdkSID, userAgent string) (PasswordTokens, *MFAChallenge, error) {
	resp, err := c.doJSON(ctx, http.MethodPut, c.authBase()+"/api/v1/authorization", body, cookies, sdkSID, userAgent)
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("auth put: %w", err)
	}
	defer resp.Body.Close()
	raw, err := readPasswordResponse(resp.Body)
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("auth put response: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return PasswordTokens{}, nil, ErrPasswordRateLimit
	}
	if isMFASchemaRejection(resp.StatusCode, body, raw) {
		return PasswordTokens{}, nil, &mfaSchemaRejectedError{}
	}
	if resp.StatusCode != http.StatusOK {
		return PasswordTokens{}, nil, fmt.Errorf("auth put: %s", resp.Status)
	}

	var out struct {
		Type    string `json:"type"`
		Error   string `json:"error"`
		Success struct {
			LoginToken string `json:"login_token"`
		} `json:"success"`
		Multifactor struct {
			Email   string   `json:"email"`
			Method  string   `json:"method"`
			Methods []string `json:"methods"`
		} `json:"multifactor"`
		Response struct {
			Parameters struct {
				URI string `json:"uri"`
			} `json:"parameters"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("auth put decode: %w", err)
	}

	isMFA := out.Type == "multifactor" ||
		out.Multifactor.Method != "" ||
		out.Multifactor.Email != "" ||
		len(out.Multifactor.Methods) > 0
	if isMFA {
		method := out.Multifactor.Method
		if method == "" && len(out.Multifactor.Methods) > 0 {
			method = out.Multifactor.Methods[0]
		}
		ch := &MFAChallenge{
			Email:     out.Multifactor.Email,
			Method:    method,
			Methods:   append([]string(nil), out.Multifactor.Methods...),
			cookies:   copyCookies(cookies),
			sdkSID:    sdkSID,
			userAgent: userAgent,
		}
		if out.Error == "invalid_code" {
			return PasswordTokens{}, ch, ErrPasswordInvalidCode
		}
		return PasswordTokens{}, ch, nil
	}
	switch {
	case out.Error == "auth_failure", out.Error == "invalid_credentials":
		return PasswordTokens{}, nil, ErrPasswordInvalid
	case out.Error == "rate_limited":
		return PasswordTokens{}, nil, ErrPasswordRateLimit
	case out.Type == "success" && out.Success.LoginToken != "":
		tok, err := c.exchangeLoginToken(ctx, out.Success.LoginToken)
		return tok, nil, err
	case out.Response.Parameters.URI != "":
		access, id, err := ParseRedirectURL(out.Response.Parameters.URI)
		if err != nil {
			return PasswordTokens{}, nil, err
		}
		outTok := PasswordTokens{AccessToken: access, IDToken: id}
		if ssid := cookies["ssid"]; ssid != "" {
			outTok.SessionCookie = "ssid=" + ssid
		}
		return outTok, nil, nil
	default:
		if out.Error != "" {
			return PasswordTokens{}, nil, fmt.Errorf("riot auth: %s", out.Error)
		}
		return PasswordTokens{}, nil, fmt.Errorf("riot auth: unexpected response")
	}
}

func isMFASchemaRejection(status int, body map[string]any, raw []byte) bool {
	if status != http.StatusBadRequest || body["type"] != "multifactor" {
		return false
	}
	var response struct {
		Type             string `json:"type"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	return json.Unmarshal(raw, &response) == nil && response.Type == "error" && response.Error == "invalid_request" &&
		strings.EqualFold(strings.TrimSpace(response.ErrorDescription), mfaNonConsumingSchemaRejection)
}

func passwordAuthenticateStatusError(status, reason string) error {
	switch reason {
	case "auth_failure", "invalid_credentials":
		return ErrPasswordInvalid
	case "rate_limited":
		return ErrPasswordRateLimit
	case "invalid_request":
		return ErrPasswordCaptcha
	default:
		if isCaptchaError(reason) {
			return ErrPasswordCaptcha
		}
		return fmt.Errorf("captcha complete: %s", status)
	}
}

func readPasswordResponse(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxPasswordResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(raw) > maxPasswordResponseBody {
		return nil, errors.New("response body exceeds limit")
	}
	return raw, nil
}

func (c *PasswordClient) exchangeLoginToken(ctx context.Context, loginToken string) (PasswordTokens, error) {
	qr := &QRClient{
		HTTPClient:  c.HTTPClient,
		AuthBaseURL: c.authBase(),
		UserAgent:   c.userAgent(),
	}
	tok, err := qr.ExchangeLoginToken(ctx, loginToken)
	if err != nil {
		return PasswordTokens{}, err
	}
	return PasswordTokens{
		AccessToken:   tok.AccessToken,
		IDToken:       tok.IDToken,
		SessionCookie: tok.SessionCookie,
	}, nil
}

func (c *PasswordClient) doAuthenticateWebJSON(ctx context.Context, method, rawURL string, body any, cookies map[string]string, userAgent string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Origin", c.authenticateBase())
	req.Header.Set("Referer", c.authenticateBase()+"/")
	req.Header.Set("User-Agent", userAgent)
	if h := cookieHeader(cookies); h != "" {
		req.Header.Set("Cookie", h)
	}
	resp, err := riotNoRedirectClient(c.HTTPClient).Do(req)
	if err != nil {
		return nil, err
	}
	mergeSetCookies(cookies, resp)
	return resp, nil
}

func (c *PasswordClient) doJSON(ctx context.Context, method, rawURL string, body any, cookies map[string]string, sdkSID, userAgent string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", userAgent)
	if strings.HasPrefix(rawURL, c.authenticateBase()+"/") {
		req.Header.Set("Origin", c.authenticateBase())
		req.Header.Set("Referer", c.authenticateBase()+"/")
	}
	if sdkSID != "" {
		req.Header.Set("baggage", "sdksid="+sdkSID)
	}
	if h := cookieHeader(cookies); h != "" {
		req.Header.Set("Cookie", h)
	}
	// Automatic 307/308 handling replays bytes.Reader request bodies. That
	// could resubmit an OTP or forward credentials to another host, so sensitive
	// Riot requests always surface redirects to the caller as terminal statuses.
	resp, err := riotNoRedirectClient(c.HTTPClient).Do(req)
	if err != nil {
		return nil, err
	}
	mergeSetCookies(cookies, resp)
	return resp, nil
}

func (c *PasswordClient) updateSessionCookies(sessionID string, cookies map[string]string, browserCookies map[string]*http.Cookie) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sess, ok := c.sessions[sessionID]; ok {
		sess.cookies = copyCookies(cookies)
		sess.browserCookies = copyCaptchaBrowserCookies(browserCookies)
		sess.expires = time.Now().Add(captchaSessionTTL)
	}
}

func (c *PasswordClient) updateSessionChallenge(sessionID, siteKey, rqData string, cookies map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sess, ok := c.sessions[sessionID]; ok {
		sess.siteKey = siteKey
		sess.rqData = rqData
		sess.cookies = copyCookies(cookies)
		sess.expires = time.Now().Add(captchaSessionTTL)
	}
}

func (c *PasswordClient) deleteSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, sessionID)
}

func validateCaptchaBrowserSession(sess *captchaSession, browser CaptchaBrowserSession) error {
	if sess == nil {
		return ErrCaptchaSession
	}
	if strings.TrimSpace(browser.UserAgent) == "" || browser.UserAgent != sess.userAgent {
		return fmt.Errorf("%w: captcha browser identity changed", ErrCaptchaSession)
	}
	expected := captchaBrowserCookieValues(sess.cookies)
	actual := captchaBrowserCookieValues(browser.Cookies)
	if len(actual) != len(expected) {
		return fmt.Errorf("%w: captcha browser cookie set changed", ErrCaptchaSession)
	}
	for name, want := range expected {
		if got, ok := actual[name]; !ok || got != want {
			return fmt.Errorf("%w: captcha browser cookie %s changed", ErrCaptchaSession, name)
		}
	}
	return nil
}

func captchaSessionCookies(expected map[string]string) map[string]string {
	return copyCookies(expected)
}

var captchaBrowserCookieAllowlist = map[string]struct{}{
	"authenticator.sid": {},
	"tdid":              {},
	"__cflb":            {},
	"__cf_bm":           {},
}

// captchaDiscoveryCookies keeps only cookies that a browser would send from
// auth.riotgames.com discovery to the authenticate.riotgames.com login path.
// Requiring explicit parent-domain, root-path, and Secure scope prevents a
// host-only or unrelated discovery cookie from being widened by our map jar.
func captchaDiscoveryCookies(setCookies []*http.Cookie) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(setCookies))
	for _, cookie := range setCookies {
		if cookie == nil || captchaCookieDeletes(cookie) || !cookie.Secure {
			continue
		}
		if _, allowed := captchaBrowserCookieAllowlist[cookie.Name]; !allowed {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
		if domain != "riotgames.com" || !captchaCookiePathMatches("/api/v1/login", cookie.Path) {
			continue
		}
		clone := *cookie
		out = append(out, &clone)
	}
	return out
}

func captchaCookiePathMatches(requestPath, cookiePath string) bool {
	if cookiePath == requestPath {
		return true
	}
	if cookiePath == "" || !strings.HasPrefix(requestPath, cookiePath) {
		return false
	}
	return strings.HasSuffix(cookiePath, "/") ||
		(len(requestPath) > len(cookiePath) && requestPath[len(cookiePath)] == '/')
}

func mergeCaptchaBrowserCookies(current map[string]*http.Cookie, setCookies []*http.Cookie) map[string]*http.Cookie {
	out := copyCaptchaBrowserCookies(current)
	for _, cookie := range setCookies {
		if cookie == nil {
			continue
		}
		if _, allowed := captchaBrowserCookieAllowlist[cookie.Name]; !allowed {
			continue
		}
		if captchaCookieDeletes(cookie) {
			delete(out, cookie.Name)
			continue
		}
		clone := *cookie
		out[cookie.Name] = &clone
	}
	return out
}

func captchaBrowserCookieValues(in map[string]string) map[string]string {
	out := make(map[string]string)
	for name, value := range in {
		if _, allowed := captchaBrowserCookieAllowlist[name]; allowed {
			out[name] = value
		}
	}
	return out
}

// captchaBrowserCookieSync returns the complete active cookie set plus any
// deletion tombstones Riot issued. It also clears allowlisted cookies retained
// by Chrome that do not belong to the new canonical Riot session.
func captchaBrowserCookieSync(active map[string]*http.Cookie, setCookies []*http.Cookie, browser, canonical map[string]string, reset bool) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(active)+len(setCookies))
	deleted := make(map[string]struct{})
	for _, cookie := range setCookies {
		if cookie == nil || !captchaCookieDeletes(cookie) {
			continue
		}
		if _, allowed := captchaBrowserCookieAllowlist[cookie.Name]; !allowed {
			continue
		}
		clone := *cookie
		out = append(out, &clone)
		deleted[cookie.Name] = struct{}{}
	}
	expected := captchaBrowserCookieValues(canonical)
	for name := range captchaBrowserCookieValues(browser) {
		if _, keep := expected[name]; keep && !reset {
			continue
		}
		if _, already := deleted[name]; already && !reset {
			continue
		}
		out = append(out,
			&http.Cookie{Name: name, Value: "", Path: "/", Secure: true, HttpOnly: true, MaxAge: -1},
			&http.Cookie{Name: name, Value: "", Domain: "riotgames.com", Path: "/", Secure: true, HttpOnly: true, MaxAge: -1},
		)
	}
	out = append(out, captchaBrowserCookieSlice(active)...)
	return out
}

func captchaCookieDeletes(cookie *http.Cookie) bool {
	return cookie != nil && (cookie.Value == "" || cookie.MaxAge < 0 ||
		(!cookie.Expires.IsZero() && cookie.Expires.Before(time.Now())))
}

func copyCaptchaBrowserCookies(in map[string]*http.Cookie) map[string]*http.Cookie {
	out := make(map[string]*http.Cookie, len(in))
	for name, cookie := range in {
		if cookie == nil {
			continue
		}
		clone := *cookie
		out[name] = &clone
	}
	return out
}

func captchaBrowserCookieSlice(in map[string]*http.Cookie) []*http.Cookie {
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*http.Cookie, 0, len(names))
	for _, name := range names {
		clone := *in[name]
		out = append(out, &clone)
	}
	return out
}

func (c *PasswordClient) purgeExpiredLocked() {
	now := time.Now()
	for id, s := range c.sessions {
		if now.After(s.expires) {
			delete(c.sessions, id)
		}
	}
}

func (c *PasswordClient) refreshClientMeta(ctx context.Context) {
	c.metaOnce.Do(func() {
		c.metaMu.RLock()
		configured := c.UserAgent != "" && c.SDKVersion != ""
		c.metaMu.RUnlock()
		if configured {
			return
		}
		meta, err := FetchClientMeta(ctx, c.HTTPClient)
		if err != nil {
			// Metadata is an optimization only. Keep the built-in Riot Client
			// values and do not make concurrent logins retry an outage serially.
			return
		}
		c.metaMu.Lock()
		defer c.metaMu.Unlock()
		if c.UserAgent == "" {
			build := meta.RiotClientBuild
			if build == "" {
				build = "111.0.0.3261.5663"
			}
			c.UserAgent = "RiotClient/" + build + " rso-auth (Windows;10;;Professional, x64)"
		}
		if c.SDKVersion == "" {
			// riotClientVersion looks like "release-XX-YY-shipping-N-BUILD"; sdk is the tail.
			parts := strings.SplitN(meta.RiotClientVersion, ".", 2)
			if len(parts) == 2 && parts[1] != "" {
				c.SDKVersion = parts[1]
			}
		}
	})
}

func (c *PasswordClient) authBase() string {
	if c.AuthBaseURL != "" {
		return strings.TrimSuffix(c.AuthBaseURL, "/")
	}
	return "https://auth.riotgames.com"
}

func (c *PasswordClient) authenticateBase() string {
	if c.AuthenticateBaseURL != "" {
		return strings.TrimSuffix(c.AuthenticateBaseURL, "/")
	}
	return defaultAuthenticateBaseURL
}

func (c *PasswordClient) userAgent() string {
	c.metaMu.RLock()
	defer c.metaMu.RUnlock()
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return "RiotClient/111.0.0.3261.5663 rso-auth (Windows;10;;Professional, x64)"
}

func (c *PasswordClient) sdkVersion() string {
	c.metaMu.RLock()
	defer c.metaMu.RUnlock()
	if c.SDKVersion != "" {
		return c.SDKVersion
	}
	return "02-shipping-10-5229475"
}

func extractHCaptcha(raw []byte) (siteKey, rqData string, ok bool) {
	var out struct {
		Captcha struct {
			HCaptcha struct {
				Key  string `json:"key"`
				Data string `json:"data"`
			} `json:"hcaptcha"`
		} `json:"captcha"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", false
	}
	if out.Captcha.HCaptcha.Key == "" || out.Captcha.HCaptcha.Data == "" {
		return "", "", false
	}
	return out.Captcha.HCaptcha.Key, out.Captcha.HCaptcha.Data, true
}

func hasHCaptcha(raw []byte) bool {
	_, _, ok := extractHCaptcha(raw)
	return ok
}

func isCaptchaError(err string) bool {
	e := strings.ToLower(strings.TrimSpace(err))
	if e == "" {
		return false
	}
	return strings.Contains(e, "captcha")
}

func authenticateErrorReason(raw []byte) string {
	var out struct {
		Error string `json:"error"`
		Type  string `json:"type"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Error != "" {
		return out.Error
	}
	if out.Type != "" {
		return out.Type
	}
	return "rejected"
}

func copyCookies(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
