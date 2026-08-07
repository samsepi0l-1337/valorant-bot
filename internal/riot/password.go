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

// CaptchaRetryError means Riot rejected the token but issued a fresh challenge.
// The browser page should re-render the widget with SiteKey/RQData.
type CaptchaRetryError struct {
	SiteKey string
	RQData  string
	Reason  string
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

// CaptchaChallenge is the hCaptcha widget data for browser solving.
type CaptchaChallenge struct {
	SessionID string
	SiteKey   string
	RQData    string
}

// MFAChallenge means Riot needs a second factor before handing out tokens.
type MFAChallenge struct {
	Email   string
	Method  string
	Methods []string
	cookies map[string]string
	sdkSID  string
	// authenticate is true when MFA must be submitted to authenticate.riotgames.com.
	authenticate bool
}

type captchaSession struct {
	username string
	password string
	cookies  map[string]string
	sdkSID   string
	siteKey  string
	rqData   string
	expires  time.Time
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

// BeginCaptcha starts the Riot authenticate login and returns hCaptcha widget data.
// Credentials are held only in memory until login completes or the session expires.
func (c *PasswordClient) BeginCaptcha(ctx context.Context, username, password string) (CaptchaChallenge, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return CaptchaChallenge{}, ErrPasswordInvalid
	}
	c.refreshClientMeta(ctx)

	cookies := map[string]string{}
	sdkSID := randomHex(16)
	body := map[string]any{
		"clientId":   "riot-client",
		"language":   "",
		"platform":   "windows",
		"remember":   false,
		"type":       "auth",
		"sdkVersion": c.sdkVersion(),
		"riot_identity": map[string]any{
			"language": "ko_KR",
			"state":    "auth",
		},
	}
	resp, err := c.doJSON(ctx, http.MethodPost, c.authenticateBase()+"/api/v1/login", body, cookies, sdkSID)
	if err != nil {
		return CaptchaChallenge{}, fmt.Errorf("captcha begin: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return CaptchaChallenge{}, fmt.Errorf("captcha begin: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
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

	sessionID := randomHex(16)
	c.mu.Lock()
	if c.sessions == nil {
		c.sessions = make(map[string]*captchaSession)
	}
	c.purgeExpiredLocked()
	c.sessions[sessionID] = &captchaSession{
		username: username,
		password: password,
		cookies:  cookies,
		sdkSID:   sdkSID,
		siteKey:  siteKey,
		rqData:   rqData,
		expires:  time.Now().Add(captchaSessionTTL),
	}
	c.mu.Unlock()

	return CaptchaChallenge{SessionID: sessionID, SiteKey: siteKey, RQData: rqData}, nil
}

// CompleteCaptcha submits the browser hCaptcha token with stored credentials.
// On MFA it returns a non-nil *MFAChallenge.
// On captcha rejection with a fresh challenge it returns *CaptchaRetryError and keeps the session.
func (c *PasswordClient) CompleteCaptcha(ctx context.Context, sessionID, captchaToken string) (PasswordTokens, *MFAChallenge, error) {
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
	// Copy credentials/cookies; keep session for captcha retry.
	username := sess.username
	password := sess.password
	cookies := copyCookies(sess.cookies)
	sdkSID := sess.sdkSID
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
	resp, err := c.doJSON(ctx, http.MethodPut, c.authenticateBase()+"/api/v1/login", body, cookies, sdkSID)
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("captcha complete: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// Persist cookies from the attempt back into the session for retries/MFA.
	c.updateSessionCookies(sessionID, cookies)

	if resp.StatusCode == http.StatusTooManyRequests {
		return PasswordTokens{}, nil, ErrPasswordRateLimit
	}

	tokens, mfa, perr := c.parseAuthenticateLogin(ctx, raw, cookies, true, sdkSID)
	if perr == nil {
		c.deleteSession(sessionID)
		return tokens, mfa, nil
	}

	reason := authenticateErrorReason(raw)
	siteKey, rqData, hasChallenge := extractHCaptcha(raw)

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
			return PasswordTokens{}, nil, &CaptchaRetryError{SiteKey: siteKey, RQData: rqData, Reason: reason}
		}
		c.deleteSession(sessionID)
		log.Printf("riot password rejected after captcha (%s)", reason)
		return PasswordTokens{}, nil, ErrPasswordInvalid
	}

	// Captcha / request rejected — keep session when Riot sends a new widget.
	if hasChallenge && (isCaptchaError(reason) || reason == "invalid_request" || errors.Is(perr, ErrPasswordCaptcha)) {
		c.updateSessionChallenge(sessionID, siteKey, rqData, cookies)
		log.Printf("riot captcha rejected (%s); issuing retry challenge", reason)
		return PasswordTokens{}, nil, &CaptchaRetryError{SiteKey: siteKey, RQData: rqData, Reason: reason}
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

	bodies := []map[string]any{
		{
			"type": "multifactor",
			"multifactor": map[string]any{
				"otp":            code,
				"rememberDevice": true,
			},
		},
		{
			"type":           "multifactor",
			"code":           code,
			"rememberDevice": true,
		},
	}

	var lastErr error
	for _, body := range bodies {
		var (
			tokens PasswordTokens
			mfa    *MFAChallenge
			err    error
		)
		if challenge.authenticate {
			tokens, mfa, err = c.putAuthenticate(ctx, challenge.cookies, body, challenge.sdkSID)
		} else {
			tokens, mfa, err = c.putAuth(ctx, challenge.cookies, body, challenge.sdkSID)
		}
		if err == nil && mfa == nil {
			return tokens, nil
		}
		if errors.Is(err, ErrPasswordInvalidCode) || mfa != nil {
			return PasswordTokens{}, ErrPasswordInvalidCode
		}
		if errors.Is(err, ErrPasswordInvalid) {
			lastErr = ErrPasswordInvalidCode
			continue
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrPasswordInvalidCode
	}
	return PasswordTokens{}, lastErr
}

func (c *PasswordClient) putAuthenticate(ctx context.Context, cookies map[string]string, body map[string]any, sdkSID string) (PasswordTokens, *MFAChallenge, error) {
	resp, err := c.doJSON(ctx, http.MethodPut, c.authenticateBase()+"/api/v1/login", body, cookies, sdkSID)
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("authenticate put: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return PasswordTokens{}, nil, ErrPasswordRateLimit
	}
	tokens, mfa, perr := c.parseAuthenticateLogin(ctx, raw, cookies, true, sdkSID)
	if perr != nil && resp.StatusCode != http.StatusOK {
		return PasswordTokens{}, nil, perr
	}
	return tokens, mfa, perr
}

func (c *PasswordClient) parseAuthenticateLogin(ctx context.Context, raw []byte, cookies map[string]string, authenticateMFA bool, sdkSID string) (PasswordTokens, *MFAChallenge, error) {
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

func (c *PasswordClient) putAuth(ctx context.Context, cookies map[string]string, body map[string]any, sdkSID string) (PasswordTokens, *MFAChallenge, error) {
	resp, err := c.doJSON(ctx, http.MethodPut, c.authBase()+"/api/v1/authorization", body, cookies, sdkSID)
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("auth put: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return PasswordTokens{}, nil, ErrPasswordRateLimit
	}
	if resp.StatusCode != http.StatusOK {
		return PasswordTokens{}, nil, fmt.Errorf("auth put: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
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
			Email:   out.Multifactor.Email,
			Method:  method,
			Methods: append([]string(nil), out.Multifactor.Methods...),
			cookies: copyCookies(cookies),
			sdkSID:  sdkSID,
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

func (c *PasswordClient) doJSON(ctx context.Context, method, rawURL string, body any, cookies map[string]string, sdkSID string) (*http.Response, error) {
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
	req.Header.Set("User-Agent", c.userAgent())
	if sdkSID != "" {
		req.Header.Set("baggage", "sdksid="+sdkSID)
	}
	if h := cookieHeader(cookies); h != "" {
		req.Header.Set("Cookie", h)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	mergeSetCookies(cookies, resp)
	return resp, nil
}

func (c *PasswordClient) updateSessionCookies(sessionID string, cookies map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sess, ok := c.sessions[sessionID]; ok {
		sess.cookies = copyCookies(cookies)
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
