package riot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var (
	ErrPasswordInvalid     = errors.New("invalid riot username or password")
	ErrPasswordRateLimit   = errors.New("riot login rate limited; try again later")
	ErrPasswordCaptcha     = errors.New("riot requires captcha; use Riot Mobile QR instead")
	ErrPasswordInvalidCode = errors.New("invalid multifactor code")
)

// PasswordTokens are session tokens from username/password (or MFA) login.
type PasswordTokens struct {
	AccessToken   string
	IDToken       string
	SessionCookie string
}

// MFAChallenge means Riot needs a second factor before handing out tokens.
// Method is typically "email" or "authenticator" (Riot Mobile / TOTP).
type MFAChallenge struct {
	Email   string
	Method  string
	Methods []string
	cookies map[string]string
}

// PasswordClient performs Riot ID/password auth from Discord modal credentials.
type PasswordClient struct {
	HTTPClient  *http.Client
	AuthBaseURL string
	UserAgent   string
}

// NewPasswordClient builds a client against Riot production endpoints.
func NewPasswordClient(httpClient *http.Client) *PasswordClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &PasswordClient{
		HTTPClient:  httpClient,
		AuthBaseURL: "https://auth.riotgames.com",
		UserAgent:   defaultQRUserAgent,
	}
}

// LoginWithPassword authenticates with Riot username/password.
// On MFA it returns a non-nil *MFAChallenge.
func (c *PasswordClient) LoginWithPassword(ctx context.Context, username, password string) (PasswordTokens, *MFAChallenge, error) {
	cookies := map[string]string{}
	if err := c.prepareAuthCookies(ctx, cookies); err != nil {
		return PasswordTokens{}, nil, err
	}
	body := map[string]any{
		"type":     "auth",
		"username": strings.TrimSpace(username),
		"password": password,
		"remember": true,
		"language": "ko_KR",
	}
	return c.putAuth(ctx, cookies, body)
}

// SubmitMFA continues a password login after the user provides an OTP
// (email or Riot Mobile / authenticator app).
func (c *PasswordClient) SubmitMFA(ctx context.Context, challenge *MFAChallenge, code string) (PasswordTokens, error) {
	if challenge == nil || challenge.cookies == nil {
		return PasswordTokens{}, errors.New("missing mfa challenge")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return PasswordTokens{}, ErrPasswordInvalidCode
	}

	// Documented body (valorant-api-docs).
	tokens, mfa, err := c.putAuth(ctx, challenge.cookies, map[string]any{
		"type": "multifactor",
		"multifactor": map[string]any{
			"otp":            code,
			"rememberDevice": true,
		},
	})
	if err == nil && mfa == nil {
		return tokens, nil
	}
	if errors.Is(err, ErrPasswordInvalidCode) {
		return PasswordTokens{}, err
	}

	// Older clients send a flat {type,code,rememberDevice} body.
	tokens, mfa, err = c.putAuth(ctx, challenge.cookies, map[string]any{
		"type":           "multifactor",
		"code":           code,
		"rememberDevice": true,
	})
	if err != nil {
		return PasswordTokens{}, err
	}
	if mfa != nil {
		return PasswordTokens{}, ErrPasswordInvalidCode
	}
	return tokens, nil
}

func (c *PasswordClient) prepareAuthCookies(ctx context.Context, cookies map[string]string) error {
	body := map[string]any{
		"client_id":     "riot-client",
		"nonce":         "1",
		"redirect_uri":  qrRedirectURI,
		"response_type": "token id_token",
		"scope":         qrScope,
	}
	resp, err := c.doJSON(ctx, http.MethodPost, c.authBase()+"/api/v1/authorization", body, cookies)
	if err != nil {
		return fmt.Errorf("auth cookies: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("auth cookies: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *PasswordClient) putAuth(ctx context.Context, cookies map[string]string, body map[string]any) (PasswordTokens, *MFAChallenge, error) {
	resp, err := c.doJSON(ctx, http.MethodPut, c.authBase()+"/api/v1/authorization", body, cookies)
	if err != nil {
		return PasswordTokens{}, nil, fmt.Errorf("auth put: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return PasswordTokens{}, nil, ErrPasswordRateLimit
	default:
		msg := strings.TrimSpace(string(raw))
		if strings.Contains(strings.ToLower(msg), "captcha") {
			return PasswordTokens{}, nil, ErrPasswordCaptcha
		}
		return PasswordTokens{}, nil, fmt.Errorf("auth put: %s: %s", resp.Status, msg)
	}

	var out struct {
		Type        string `json:"type"`
		Error       string `json:"error"`
		Success     struct {
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

	mfaChallenge := func() *MFAChallenge {
		method := out.Multifactor.Method
		if method == "" && len(out.Multifactor.Methods) > 0 {
			method = out.Multifactor.Methods[0]
		}
		return &MFAChallenge{
			Email:   out.Multifactor.Email,
			Method:  method,
			Methods: append([]string(nil), out.Multifactor.Methods...),
			cookies: copyCookies(cookies),
		}
	}

	// Multifactor must win over error=auth_failure-style fields. Invalid OTP
	// responses keep type=multifactor with error=invalid_code.
	isMFA := out.Type == "multifactor" ||
		out.Multifactor.Method != "" ||
		out.Multifactor.Email != "" ||
		len(out.Multifactor.Methods) > 0

	switch {
	case isMFA:
		ch := mfaChallenge()
		if out.Error == "invalid_code" {
			return PasswordTokens{}, ch, ErrPasswordInvalidCode
		}
		return PasswordTokens{}, ch, nil
	case out.Error == "auth_failure", out.Error == "invalid_credentials":
		return PasswordTokens{}, nil, ErrPasswordInvalid
	case strings.Contains(strings.ToLower(out.Error), "captcha"):
		return PasswordTokens{}, nil, ErrPasswordCaptcha
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

func (c *PasswordClient) doJSON(ctx context.Context, method, rawURL string, body any, cookies map[string]string) (*http.Response, error) {
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
	req.Header.Set("User-Agent", c.userAgent())
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

func (c *PasswordClient) authBase() string {
	if c.AuthBaseURL != "" {
		return strings.TrimSuffix(c.AuthBaseURL, "/")
	}
	return "https://auth.riotgames.com"
}

func (c *PasswordClient) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return defaultQRUserAgent
}

func copyCookies(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
