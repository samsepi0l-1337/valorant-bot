package riot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Riot Mobile QR login ("스캔해서 로그인"). The Riot Client asks rso-authenticator
// for a QR session, the user approves it in Riot Mobile, and the resulting
// login_token is exchanged for RSO tokens — no browser, no localhost redirect.
const (
	defaultAuthenticateBaseURL = "https://authenticate.riotgames.com"
	defaultQRLoginBaseURL      = "https://qrlogin.riotgames.com"
	defaultQRUserAgent         = "RiotGamesApi/24.9.1.4445 rso-authenticator (Windows;10;;Professional, x64) riot_client/0"
	defaultQRLanguage          = "ko_KR"

	// qrRedirectURI is the RSO redirect registered for client_id=riot-client.
	// Nothing listens on it: Riot returns the tokens in the response body.
	qrRedirectURI = "http://localhost/redirect"
	qrScope       = "openid link ban lol_region account"
)

var (
	// ErrQRPending means the user has not approved the QR code in Riot Mobile yet.
	ErrQRPending = errors.New("qr login pending")
	// ErrQRExpired means the QR session is no longer usable and must be restarted.
	ErrQRExpired = errors.New("qr login expired")
)

// QRSession is one in-flight Riot Mobile QR login.
type QRSession struct {
	// LoginURL is what the user scans (or taps on mobile) to approve the login.
	LoginURL string
	SUUID    string

	cookies map[string]string
	sdkSID  string
}

// QRTokens are the RSO tokens obtained after a QR login is approved.
type QRTokens struct {
	AccessToken string
	IDToken     string
	// SessionCookie is a `ssid=...` Cookie header for CookieReauth, empty when
	// Riot did not hand out a persistent session.
	SessionCookie string
}

// QRClient talks to Riot's QR login endpoints.
type QRClient struct {
	HTTPClient *http.Client

	AuthBaseURL         string
	AuthenticateBaseURL string
	QRLoginBaseURL      string
	Language            string
	UserAgent           string
}

// NewQRClient builds a QRClient against Riot's production endpoints.
func NewQRClient(httpClient *http.Client) *QRClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &QRClient{
		HTTPClient:          httpClient,
		AuthBaseURL:         "https://auth.riotgames.com",
		AuthenticateBaseURL: defaultAuthenticateBaseURL,
		QRLoginBaseURL:      defaultQRLoginBaseURL,
		Language:            defaultQRLanguage,
		UserAgent:           defaultQRUserAgent,
	}
}

// StartQRSession asks Riot for a QR login and returns the URL to display.
func (c *QRClient) StartQRSession(ctx context.Context) (*QRSession, error) {
	sess := &QRSession{
		cookies: map[string]string{},
		sdkSID:  randomHex(16),
	}

	// Riot's client hits the OIDC discovery document first; it seeds the
	// session cookies that authenticate.riotgames.com expects.
	discovery, err := c.do(ctx, http.MethodGet, c.authBase()+"/.well-known/openid-configuration", nil, sess)
	if err != nil {
		return nil, fmt.Errorf("qr discovery: %w", err)
	}
	discovery.Body.Close()

	body := map[string]any{
		"client_id": "riot-client",
		"language":  c.language(),
		"platform":  "windows",
		"remember":  false,
		"type":      "auth",
		"qrcode":    map[string]any{},
	}
	resp, err := c.do(ctx, http.MethodPost, c.authenticateBase()+"/api/v1/login", body, sess)
	if err != nil {
		return nil, fmt.Errorf("qr start: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("qr start: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var out struct {
		Cluster   string      `json:"cluster"`
		SUUID     string      `json:"suuid"`
		Timestamp json.Number `json:"timestamp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("qr start: %w", err)
	}
	if out.Cluster == "" || out.SUUID == "" || out.Timestamp == "" {
		return nil, errors.New("qr start: Riot returned no QR session (cluster/suuid/timestamp)")
	}

	q := url.Values{}
	q.Set("cluster", out.Cluster)
	q.Set("suuid", out.SUUID)
	q.Set("timestamp", out.Timestamp.String())
	q.Set("utm_source", "riotclient")
	q.Set("utm_medium", "client")
	q.Set("utm_campaign", "qrlogin-riotmobile")

	sess.SUUID = out.SUUID
	sess.LoginURL = c.qrLoginBase() + "/riotmobile?" + q.Encode()
	return sess, nil
}

// PollQRSession checks once whether the QR code has been approved.
// It returns ErrQRPending while waiting and ErrQRExpired once the session dies.
func (c *QRClient) PollQRSession(ctx context.Context, sess *QRSession) (loginToken string, err error) {
	if sess == nil {
		return "", errors.New("qr poll: nil session")
	}
	resp, err := c.do(ctx, http.MethodGet, c.authenticateBase()+"/api/v1/login", nil, sess)
	if err != nil {
		return "", fmt.Errorf("qr poll: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return "", ErrQRExpired
	default:
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("qr poll: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var out struct {
		Type    string `json:"type"`
		Error   string `json:"error"`
		Success struct {
			LoginToken string `json:"login_token"`
		} `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("qr poll: %w", err)
	}

	switch out.Type {
	case "success":
		if out.Success.LoginToken == "" {
			return "", errors.New("qr poll: success without login_token")
		}
		return out.Success.LoginToken, nil
	case "error":
		return "", fmt.Errorf("%w: %s", ErrQRExpired, out.Error)
	default:
		return "", ErrQRPending
	}
}

// ExchangeLoginToken turns an approved QR login_token into RSO tokens.
func (c *QRClient) ExchangeLoginToken(ctx context.Context, loginToken string) (QRTokens, error) {
	sess := &QRSession{cookies: map[string]string{}, sdkSID: randomHex(16)}

	tokenBody := map[string]any{
		"authentication_type": nil,
		"code_verifier":       "",
		"login_token":         loginToken,
		// Persisting yields an ssid cookie so daily /shop runs can reauth
		// without another QR scan.
		"persist_login": true,
	}
	resp, err := c.do(ctx, http.MethodPost, c.authBase()+"/api/v1/login-token", tokenBody, sess)
	if err != nil {
		return QRTokens{}, fmt.Errorf("login-token: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return QRTokens{}, fmt.Errorf("login-token: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	authzBody := map[string]any{
		"acr_values":            "",
		"claims":                "",
		"client_id":             "riot-client",
		"code_challenge":        "",
		"code_challenge_method": "",
		"nonce":                 randomHex(16),
		"redirect_uri":          qrRedirectURI,
		"response_type":         "token id_token",
		"scope":                 qrScope,
	}
	authzResp, err := c.do(ctx, http.MethodPost, c.authBase()+"/api/v1/authorization", authzBody, sess)
	if err != nil {
		return QRTokens{}, fmt.Errorf("authorization: %w", err)
	}
	defer authzResp.Body.Close()
	if authzResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(authzResp.Body)
		return QRTokens{}, fmt.Errorf("authorization: %s: %s", authzResp.Status, strings.TrimSpace(string(b)))
	}

	var out struct {
		Response struct {
			Parameters struct {
				URI string `json:"uri"`
			} `json:"parameters"`
		} `json:"response"`
	}
	if err := json.NewDecoder(authzResp.Body).Decode(&out); err != nil {
		return QRTokens{}, fmt.Errorf("authorization: %w", err)
	}

	access, id, err := ParseRedirectURL(out.Response.Parameters.URI)
	if err != nil {
		return QRTokens{}, fmt.Errorf("authorization: %w", err)
	}

	tokens := QRTokens{AccessToken: access, IDToken: id}
	if ssid := sess.cookies["ssid"]; ssid != "" {
		tokens.SessionCookie = "ssid=" + ssid
	}
	return tokens, nil
}

func (c *QRClient) do(ctx context.Context, method, rawURL string, body any, sess *QRSession) (*http.Response, error) {
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
	if sess.cookies == nil {
		sess.cookies = map[string]string{}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("baggage", "sdksid="+sess.sdkSID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h := cookieHeader(sess.cookies); h != "" {
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
	mergeSetCookies(sess.cookies, resp)
	return resp, nil
}

func mergeSetCookies(dst map[string]string, resp *http.Response) {
	for _, ck := range resp.Cookies() {
		if ck.Value == "" {
			delete(dst, ck.Name)
			continue
		}
		dst[ck.Name] = ck.Value
	}
}

func cookieHeader(cookies map[string]string) string {
	if len(cookies) == 0 {
		return ""
	}
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+cookies[name])
	}
	return strings.Join(parts, "; ")
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

func (c *QRClient) authBase() string {
	if c.AuthBaseURL != "" {
		return strings.TrimSuffix(c.AuthBaseURL, "/")
	}
	return "https://auth.riotgames.com"
}

func (c *QRClient) authenticateBase() string {
	if c.AuthenticateBaseURL != "" {
		return strings.TrimSuffix(c.AuthenticateBaseURL, "/")
	}
	return defaultAuthenticateBaseURL
}

func (c *QRClient) qrLoginBase() string {
	if c.QRLoginBaseURL != "" {
		return strings.TrimSuffix(c.QRLoginBaseURL, "/")
	}
	return defaultQRLoginBaseURL
}

func (c *QRClient) language() string {
	if c.Language != "" {
		return c.Language
	}
	return defaultQRLanguage
}

func (c *QRClient) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return defaultQRUserAgent
}
