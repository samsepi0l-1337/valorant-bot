package authweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
)

const (
	riotBrowserDiscoveryInterval = 25 * time.Millisecond
	riotBrowserInputCommitDelay  = 50 * time.Millisecond
	riotBrowserResponseLimit     = 1 << 20
	riotBrowserNavigationRetries = 8
)

type browserPasswordAuthClient interface {
	BrowserAuthorizeURL(state string) (string, error)
	AdoptBrowserLogin(ctx context.Context, responseBody []byte, cookies []*http.Cookie, userAgent string) (riot.PasswordTokens, *riot.MFAChallenge, error)
}

type riotBrowserLoginResult struct {
	ResponseBody []byte
	Cookies      []*http.Cookie
	UserAgent    string
}

type riotBrowserLoginController interface {
	captchaBrowserController
	RunRiotLogin(ctx context.Context, username, password string) (riotBrowserLoginResult, error)
}

func (c *chromeBrowserController) RunRiotLogin(ctx context.Context, username, password string) (riotBrowserLoginResult, error) {
	if c == nil || strings.TrimSpace(c.profileDir) == "" {
		return riotBrowserLoginResult{}, errors.New("captcha Chrome profile is unavailable")
	}
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return riotBrowserLoginResult{}, riot.ErrPasswordInvalid
	}
	if c.devToolsPipe == nil {
		return riotBrowserLoginResult{}, errors.New("official Riot login requires a private Chrome DevTools pipe")
	}
	client := newChromeDevToolsClient(c.devToolsPipe)
	if err := client.attachRiotPage(ctx); err != nil {
		return riotBrowserLoginResult{}, err
	}
	for _, method := range []string{"Network.enable", "Runtime.enable", "Page.enable"} {
		if err := client.Call(ctx, method, map[string]any{}, nil); err != nil {
			return riotBrowserLoginResult{}, err
		}
	}
	networkEvents, err := client.SubscribeEvents(client.currentSessionID(),
		"Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished")
	if err != nil {
		return riotBrowserLoginResult{}, fmt.Errorf("watch Riot browser login: %w", err)
	}
	defer networkEvents.Close()
	if err := client.submitRiotCredentials(ctx, username, password); err != nil {
		return riotBrowserLoginResult{}, err
	}
	return client.waitForRiotLogin(ctx, networkEvents)
}

func (c *chromeDevToolsClient) attachRiotPage(ctx context.Context) error {
	ticker := time.NewTicker(riotBrowserDiscoveryInterval)
	defer ticker.Stop()
	for {
		var targets struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
			} `json:"targetInfos"`
		}
		if err := c.Call(ctx, "Target.getTargets", map[string]any{}, &targets); err != nil {
			return fmt.Errorf("discover Riot Chrome page: %w", err)
		}
		for _, target := range targets.TargetInfos {
			if target.Type != "page" || target.TargetID == "" || !allowedRiotBrowserPage(target.URL) {
				continue
			}
			var attached struct {
				SessionID string `json:"sessionId"`
			}
			if err := c.Call(ctx, "Target.attachToTarget", map[string]any{
				"targetId": target.TargetID,
				"flatten":  true,
			}, &attached); err != nil {
				return fmt.Errorf("attach Riot Chrome page: %w", err)
			}
			if strings.TrimSpace(attached.SessionID) == "" {
				return errors.New("attach Riot Chrome page: empty session")
			}
			c.setSessionID(attached.SessionID)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("discover Riot Chrome page: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func allowedRiotBrowserPage(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	host := strings.ToLower(parsed.Host)
	return host == "auth.riotgames.com" || host == RiotCaptchaHost
}

func (c *chromeDevToolsClient) submitRiotCredentials(ctx context.Context, username, password string) error {
	usernameJSON, _ := json.Marshal(username)
	passwordJSON, _ := json.Marshal(password)
	fillExpression := `(function(){
if(document.readyState !== 'complete') return {filled:false};
const roots=[document]; for(let i=0;i<roots.length;i++){for(const el of roots[i].querySelectorAll('*')){if(el.shadowRoot) roots.push(el.shadowRoot)}}
const find=(selectors)=>{for(const root of roots){for(const selector of selectors){const el=root.querySelector(selector); if(el && !el.disabled) return el}} return null};
const username=find(['input[name="username"]','input[autocomplete="username"]','input[data-testid*="username"]','input[type="text"]']);
const password=find(['input[name="password"]','input[autocomplete="current-password"]','input[data-testid*="password"]','input[type="password"]']);
if(!username || !password) return {filled:false};
const set=(el,value)=>{const setter=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set; setter.call(el,value); el.dispatchEvent(new Event('input',{bubbles:true})); el.dispatchEvent(new Event('change',{bubbles:true}))};
set(username,` + string(usernameJSON) + `); set(password,` + string(passwordJSON) + `);
return {filled:true};
})()`
	navigationRetries := 0
	for {
		var evaluated struct {
			Result struct {
				Value struct {
					Filled bool `json:"filled"`
				} `json:"value"`
			} `json:"result"`
			ExceptionDetails json.RawMessage `json:"exceptionDetails"`
		}
		err := c.Call(ctx, "Runtime.evaluate", map[string]any{
			"expression":    fillExpression,
			"returnByValue": true,
			"awaitPromise":  true,
		}, &evaluated)
		if err != nil {
			if !retryRiotBrowserNavigationError(err, &navigationRetries) {
				return fmt.Errorf("fill Riot browser login: %w", err)
			}
			if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
				return fmt.Errorf("fill Riot browser login: %w", waitErr)
			}
			continue
		}
		if len(evaluated.ExceptionDetails) != 0 {
			return errors.New("fill Riot browser login: credential injection failed")
		}
		if evaluated.Result.Value.Filled {
			break
		}
		if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
			return fmt.Errorf("fill Riot browser login: %w", waitErr)
		}
	}

	commitTimer := time.NewTimer(riotBrowserInputCommitDelay)
	defer commitTimer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("submit Riot browser login: %w", ctx.Err())
	case <-commitTimer.C:
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("submit Riot browser login: %w", err)
		}
	}

	submitExpression := `(function(){
if(document.readyState !== 'complete') return {ready:false};
const roots=[document]; for(let i=0;i<roots.length;i++){for(const el of roots[i].querySelectorAll('*')){if(el.shadowRoot) roots.push(el.shadowRoot)}}
for(const root of roots){
  const password=root.querySelector('input[name="password"],input[autocomplete="current-password"],input[data-testid*="password"],input[type="password"]');
  const form=password && password.form; const button=form ? form.querySelector('button[data-testid="btn-signin-submit"]') : null;
  if(button && !button.disabled){
    button.scrollIntoView({block:'center',inline:'center'}); const rect=button.getBoundingClientRect();
    if(rect.width > 0 && rect.height > 0) return {ready:true,x:rect.left+rect.width/2,y:rect.top+rect.height/2}
  }
}
return {ready:false};
})()`
	navigationRetries = 0
	for {
		var evaluated struct {
			Result struct {
				Value struct {
					Ready bool    `json:"ready"`
					X     float64 `json:"x"`
					Y     float64 `json:"y"`
				} `json:"value"`
			} `json:"result"`
			ExceptionDetails json.RawMessage `json:"exceptionDetails"`
		}
		err := c.Call(ctx, "Runtime.evaluate", map[string]any{
			"expression":    submitExpression,
			"returnByValue": true,
			"awaitPromise":  true,
		}, &evaluated)
		if err != nil {
			if !retryRiotBrowserNavigationError(err, &navigationRetries) {
				return fmt.Errorf("submit Riot browser login: %w", err)
			}
			if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
				return fmt.Errorf("submit Riot browser login: %w", waitErr)
			}
			continue
		}
		if len(evaluated.ExceptionDetails) != 0 {
			return errors.New("submit Riot browser login: submit click failed")
		}
		if evaluated.Result.Value.Ready {
			point := map[string]any{
				"x": evaluated.Result.Value.X, "y": evaluated.Result.Value.Y,
				"button": "left", "clickCount": 1, "pointerType": "mouse",
			}
			for _, eventType := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
				point["type"] = eventType
				if eventType == "mousePressed" {
					point["buttons"] = 1
				} else {
					point["buttons"] = 0
				}
				if err := c.Call(ctx, "Input.dispatchMouseEvent", point, nil); err != nil {
					return fmt.Errorf("submit Riot browser login: %w", err)
				}
			}
			return nil
		}
		if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
			return fmt.Errorf("submit Riot browser login: %w", waitErr)
		}
	}
}

func waitRiotBrowserDiscovery(ctx context.Context) error {
	timer := time.NewTimer(riotBrowserDiscoveryInterval)
	defer timer.Stop()
	return waitRiotBrowserDiscoveryEvent(ctx, timer.C, nil, nil)
}

// waitRiotBrowserDiscoveryEvent keeps the cancellation decision testable
// without changing the production timer. Hooks are nil outside tests.
func waitRiotBrowserDiscoveryEvent(ctx context.Context, timer <-chan time.Time, beforeSelect, afterTimer func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if beforeSelect != nil {
		beforeSelect()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer:
		if afterTimer != nil {
			afterTimer()
		}
		// Prefer cancellation even when it becomes ready with the timer.
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
}

func retryRiotBrowserNavigationError(err error, retries *int) bool {
	var protocolErr *chromeDevToolsProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Method != "Runtime.evaluate" || retries == nil {
		return false
	}
	message := strings.ToLower(protocolErr.Message)
	navigationAbort := strings.Contains(message, "execution context was destroyed") ||
		strings.Contains(message, "cannot find default execution context") ||
		strings.Contains(message, "inspected target navigated or closed") ||
		strings.Contains(message, "not attached to an active page")
	if !navigationAbort || *retries >= riotBrowserNavigationRetries {
		return false
	}
	*retries++
	return true
}

type riotBrowserRequest struct {
	method   string
	rawURL   string
	response bool
	status   int
}

func (c *chromeDevToolsClient) waitForRiotLogin(ctx context.Context, events *chromeDevToolsEventSubscription) (riotBrowserLoginResult, error) {
	requests := make(map[string]riotBrowserRequest)
	for {
		event, err := events.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return riotBrowserLoginResult{}, ctx.Err()
			}
			return riotBrowserLoginResult{}, fmt.Errorf("watch Riot browser login: %w", err)
		}
		switch event.Method {
		case "Network.requestWillBeSent":
			var params struct {
				RequestID string `json:"requestId"`
				Request   struct {
					URL    string `json:"url"`
					Method string `json:"method"`
				} `json:"request"`
			}
			if json.Unmarshal(event.Params, &params) == nil {
				requests[params.RequestID] = riotBrowserRequest{method: params.Request.Method, rawURL: params.Request.URL}
				if endpoint, hasQuery := riotBrowserLoginEndpoint(params.Request.URL); endpoint {
					log.Printf("Riot browser login request started method=%s query=%t", params.Request.Method, hasQuery)
				}
			}
		case "Network.responseReceived":
			var params struct {
				RequestID string `json:"requestId"`
				Response  struct {
					URL    string `json:"url"`
					Status int    `json:"status"`
				} `json:"response"`
			}
			if json.Unmarshal(event.Params, &params) == nil {
				request := requests[params.RequestID]
				request.response = true
				request.status = params.Response.Status
				if request.rawURL == "" {
					request.rawURL = params.Response.URL
				}
				requests[params.RequestID] = request
			}
		case "Network.loadingFinished":
			var params struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(event.Params, &params) != nil {
				continue
			}
			request := requests[params.RequestID]
			delete(requests, params.RequestID)
			endpoint, _ := riotBrowserLoginEndpoint(request.rawURL)
			if request.response && endpoint && request.status == http.StatusOK && request.method != http.MethodPut {
				body, bodyErr := c.riotResponseBody(ctx, params.RequestID)
				if bodyErr != nil {
					log.Printf("Riot browser login discovery response unavailable: %v", bodyErr)
				} else {
					responseType, hasCaptcha := riotBrowserResponseSummary(body)
					log.Printf("Riot browser login discovery response method=%s type=%q captcha=%t", request.method, responseType, hasCaptcha)
				}
				continue
			}
			if request.method != http.MethodPut || !request.response || !isRiotBrowserLoginURL(request.rawURL) {
				continue
			}
			if request.status != http.StatusOK {
				log.Printf("Riot browser login response rejected status=%d", request.status)
				continue
			}
			body, err := c.riotResponseBody(ctx, params.RequestID)
			if err != nil {
				return riotBrowserLoginResult{}, err
			}
			if !riotBrowserLoginTerminal(body) {
				log.Printf("Riot browser CAPTCHA challenge response received")
				continue
			}
			cookies, err := c.riotBrowserCookies(ctx)
			if err != nil {
				return riotBrowserLoginResult{}, err
			}
			userAgent, err := c.riotBrowserUserAgent(ctx)
			if err != nil {
				return riotBrowserLoginResult{}, err
			}
			return riotBrowserLoginResult{ResponseBody: body, Cookies: cookies, UserAgent: userAgent}, nil
		}
	}
}

func isRiotBrowserLoginURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(parsed.Scheme, "https") &&
		strings.EqualFold(parsed.Host, RiotCaptchaHost) && parsed.Path == "/api/v1/login" &&
		parsed.RawQuery == "" && parsed.Fragment == ""
}

func riotBrowserLoginEndpoint(rawURL string) (bool, bool) {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(parsed.Scheme, "https") &&
			strings.EqualFold(parsed.Host, RiotCaptchaHost) && parsed.Path == "/api/v1/login",
		parsed != nil && parsed.RawQuery != ""
}

func riotBrowserLoginTerminal(body []byte) bool {
	if len(body) == 0 || len(body) > riotBrowserResponseLimit {
		return false
	}
	var response struct {
		Type        string          `json:"type"`
		Error       string          `json:"error"`
		Success     json.RawMessage `json:"success"`
		Multifactor json.RawMessage `json:"multifactor"`
	}
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	hasCaptcha := bytes.Contains(bytes.ToLower(body), []byte(`"hcaptcha"`))
	if response.Type == "success" || response.Type == "multifactor" || len(response.Multifactor) > 2 || len(response.Success) > 2 {
		return true
	}
	return response.Error != "" && !hasCaptcha
}

func riotBrowserResponseSummary(body []byte) (string, bool) {
	var response struct {
		Type    string          `json:"type"`
		Captcha json.RawMessage `json:"captcha"`
	}
	if json.Unmarshal(body, &response) != nil {
		return "", false
	}
	return response.Type, len(response.Captcha) > 2 || bytes.Contains(bytes.ToLower(body), []byte(`"hcaptcha"`))
}

func (c *chromeDevToolsClient) riotResponseBody(ctx context.Context, requestID string) ([]byte, error) {
	var response struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
	}
	if err := c.Call(ctx, "Network.getResponseBody", map[string]any{"requestId": requestID}, &response); err != nil {
		return nil, err
	}
	body := []byte(response.Body)
	if response.Base64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(response.Body)
		if err != nil {
			return nil, fmt.Errorf("decode Riot browser response: %w", err)
		}
		body = decoded
	}
	if len(body) > riotBrowserResponseLimit {
		return nil, errors.New("Riot browser response exceeds limit")
	}
	return body, nil
}

func (c *chromeDevToolsClient) riotBrowserUserAgent(ctx context.Context) (string, error) {
	var evaluated struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    "navigator.userAgent",
		"returnByValue": true,
	}, &evaluated); err != nil {
		return "", err
	}
	userAgent := strings.TrimSpace(evaluated.Result.Value)
	if userAgent == "" {
		return "", errors.New("Riot browser user-agent is empty")
	}
	return userAgent, nil
}

func (c *chromeDevToolsClient) riotBrowserCookies(ctx context.Context) ([]*http.Cookie, error) {
	var response struct {
		Cookies []struct {
			Name     string  `json:"name"`
			Value    string  `json:"value"`
			Domain   string  `json:"domain"`
			Path     string  `json:"path"`
			Expires  float64 `json:"expires"`
			HTTPOnly bool    `json:"httpOnly"`
			Secure   bool    `json:"secure"`
			SameSite string  `json:"sameSite"`
		} `json:"cookies"`
	}
	if err := c.Call(ctx, "Network.getCookies", map[string]any{
		"urls": []string{"https://authenticate.riotgames.com/api/v1/login"},
	}, &response); err != nil {
		return nil, err
	}
	cookies := make([]*http.Cookie, 0, len(response.Cookies))
	for _, item := range response.Cookies {
		cookie := &http.Cookie{
			Name:     item.Name,
			Value:    item.Value,
			Domain:   item.Domain,
			Path:     item.Path,
			Secure:   item.Secure,
			HttpOnly: item.HTTPOnly,
		}
		if item.Expires > 0 {
			cookie.Expires = time.Unix(int64(item.Expires), 0)
		}
		switch strings.ToLower(item.SameSite) {
		case "strict":
			cookie.SameSite = http.SameSiteStrictMode
		case "lax":
			cookie.SameSite = http.SameSiteLaxMode
		case "none":
			cookie.SameSite = http.SameSiteNoneMode
		}
		cookies = append(cookies, cookie)
	}
	return cookies, nil
}

func (s *Server) runRiotBrowserLogin(ctx context.Context, state string, pending passwordPending, generation uint64, auth browserPasswordAuthClient, controller riotBrowserLoginController) {
	defer pending.flow.wg.Done()
	result, runErr := controller.RunRiotLogin(ctx, pending.username, pending.password)
	var (
		tokens    riot.PasswordTokens
		challenge *riot.MFAChallenge
		err       error
	)
	if runErr != nil {
		err = runErr
	} else {
		// Adoption may exchange a login token over the network. It must remain
		// outside launchMu so a reopen can take ownership and cancel this
		// generation instead of waiting behind an old upstream request.
		tokens, challenge, err = auth.AdoptBrowserLogin(ctx, result.ResponseBody, result.Cookies, result.UserAgent)
	}

	// Reopen and terminal publication are serialized by launchMu. A worker may
	// finish after its controller was closed; it must never seal the shared flow
	// or close the replacement browser.
	pending.flow.launchMu.Lock()
	defer pending.flow.launchMu.Unlock()
	if pending.flow.browserGeneration != generation || pending.flow.browser != controller {
		return
	}
	current, live := s.livePasswordState(state, "")
	if !live || current.flow != pending.flow {
		return
	}
	if err := s.claimPasswordFinalization(state, pending.flow); err != nil {
		return
	}
	closedController, closeErr := closeCaptchaBrowserLocked(pending.flow)
	s.recordCaptchaBrowserCloseResultLocked(pending.flow, closedController, closeErr, false)
	if err != nil {
		_, _ = s.publishFinalizedPasswordOutcome(state, pending.flow, passwordOutcome{err: err}, closeErr)
		return
	}
	if challenge != nil {
		mfaState, stateErr := newState()
		if stateErr != nil {
			_, _ = s.publishFinalizedPasswordOutcome(state, pending.flow, passwordOutcome{err: stateErr}, closeErr)
			return
		}
		_, _ = s.finishFinalizedPasswordMFA(state, current, mfaState, challenge, formatMFAHint(challenge), closeErr)
		return
	}
	_, _ = s.finishFinalizedPasswordAccount(pending.flow.ctx, state, current, tokens, closeErr)
}
