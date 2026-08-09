package authweb

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRiotBrowserAttachWaitsForAuthenticateOrigin(t *testing.T) {
	var targetCalls atomic.Int32
	attachedTarget := make(chan string, 1)
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	go func() {
		for {
			var command struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if browser.ReadJSON(&command) != nil {
				return
			}
			switch command.Method {
			case "Target.getTargets":
				call := targetCalls.Add(1)
				targetID := "authorize-page"
				targetURL := "https://auth.riotgames.com/authorize?nonce=test"
				if call > 1 {
					targetID = "authenticate-page"
					targetURL = "https://authenticate.riotgames.com/"
				}
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{"targetInfos": []map[string]any{{
					"targetId": targetID, "type": "page", "url": targetURL,
				}}}})
			case "Target.attachToTarget":
				targetID, _ := command.Params["targetId"].(string)
				attachedTarget <- targetID
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{"sessionId": "riot-session"}})
			}
		}
	}()

	client := newChromeDevToolsClient(host)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.attachRiotPage(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-attachedTarget; got != "authenticate-page" {
		t.Fatalf("attached target=%q, want authenticate origin after authorize redirect", got)
	}
	if got := targetCalls.Load(); got != 2 {
		t.Fatalf("Target.getTargets calls=%d, want authorize wait then authenticate attach", got)
	}
}

func TestRiotBrowserPublishesCaptchaFromDiscoveryResponse(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	client.setSessionID("riot-session")
	events, err := client.SubscribeEvents("riot-session",
		"Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()

	go func() {
		for {
			var command struct {
				ID        int64          `json:"id"`
				Method    string         `json:"method"`
				Params    map[string]any `json:"params"`
				SessionID string         `json:"sessionId"`
			}
			if browser.ReadJSON(&command) != nil {
				return
			}
			switch command.Method {
			case "Network.getResponseBody":
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{
					"body": `{"type":"auth","captcha":{"type":"hcaptcha"}}`, "base64Encoded": false,
				}})
			case "Runtime.evaluate":
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{
					"result": map[string]any{"type": "object", "value": map[string]any{
						"originOK": true, "ready": true, "x": 10, "y": 20, "width": 300, "height": 200,
					}},
				}})
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	published := make(chan riotCaptchaSurface, 1)
	done := make(chan error, 1)
	go func() {
		_, waitErr := client.waitForRiotLogin(ctx, events, func(surface riotCaptchaSurface) { published <- surface })
		done <- waitErr
	}()
	writeRiotLoginEvents(t, browser, "riot-session", "get-login", http.MethodGet)
	select {
	case surface := <-published:
		if surface != (riotCaptchaSurface{X: 10, Y: 20, Width: 300, Height: 200}) {
			t.Fatalf("published surface=%+v", surface)
		}
		cancel()
	case <-time.After(500 * time.Millisecond):
		t.Fatal("GET discovery CAPTCHA was not published to the remote viewer")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Riot login watcher did not stop after cancellation")
	}
}

func TestRiotBrowserRunsOfficialLoginInOneBrowserSession(t *testing.T) {
	var (
		mu                        sync.Mutex
		injectedCredentials       bool
		unsafeNativeSubmit        bool
		unsafeSubmitLookup        bool
		unsafeSameEvaluation      bool
		unsafeDOMClick            bool
		unsafeBeforeHydration     bool
		unsafeFillBeforeHydration bool
		unsafeTrustedSubmitBypass bool
		unsafeCaptchaInitWait     bool
		duplicatePasswordBinding  bool
		credentialEvaluations     int
		submitEvaluations         int
		unexpectedTrustedInput    bool
		curtainInstalled          bool
		unsafeCurtain             bool
		credentialsBeforeCurtain  bool
	)
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	go func() {
		for {
			var command struct {
				ID        int64          `json:"id"`
				Method    string         `json:"method"`
				Params    map[string]any `json:"params"`
				SessionID string         `json:"sessionId"`
			}
			if err := browser.ReadJSON(&command); err != nil {
				return
			}
			switch command.Method {
			case "Target.getTargets":
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{"targetInfos": []map[string]any{{
					"targetId": "riot-page", "type": "page", "url": "https://authenticate.riotgames.com/",
				}}}})
			case "Target.attachToTarget":
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{"sessionId": "riot-session"}})
			case "Network.enable", "Runtime.enable", "Page.enable":
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{}})
			case "Page.addScriptToEvaluateOnNewDocument":
				source, _ := command.Params["source"].(string)
				runImmediately, _ := command.Params["runImmediately"].(bool)
				mu.Lock()
				curtainInstalled = true
				unsafeCurtain = !runImmediately || !strings.Contains(source, "window.top!==window") ||
					!strings.Contains(source, "preventDefault") || !strings.Contains(source, "stopImmediatePropagation") ||
					!strings.Contains(source, "pointer-events:none") || !strings.Contains(source, "remote-captcha-curtain") ||
					!strings.Contains(source, "!state.trustedSubmit")
				mu.Unlock()
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{"identifier": "curtain-script"}})
			case "Runtime.evaluate":
				expression, _ := command.Params["expression"].(string)
				if strings.Contains(expression, "navigator.userAgent") {
					_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{
						"result": map[string]any{"type": "string", "value": "official-browser/1"},
					}})
					continue
				}
				injectsCredentials := strings.Contains(expression, `"browser-user"`) && strings.Contains(expression, `"browser-password"`)
				clicksSubmit := strings.Contains(expression, `.click()`)
				locatesSubmit := strings.Contains(expression, `form.querySelector('button[data-testid="btn-signin-submit"]')`)
				mu.Lock()
				if injectsCredentials {
					credentialsBeforeCurtain = credentialsBeforeCurtain || !curtainInstalled
					injectedCredentials = true
					credentialEvaluations++
					unsafeFillBeforeHydration = unsafeFillBeforeHydration ||
						!strings.Contains(expression, `document.readyState !== 'complete'`)
				}
				if locatesSubmit {
					submitEvaluations++
					duplicatePasswordBinding = strings.Count(expression, "const password=") != 1
					unsafeNativeSubmit = strings.Contains(expression, "requestSubmit")
					unsafeSubmitLookup = !locatesSubmit ||
						strings.Contains(expression, `find(['button[type="submit"]'`)
					unsafeDOMClick = unsafeDOMClick || !clicksSubmit
					unsafeBeforeHydration = unsafeBeforeHydration ||
						!strings.Contains(expression, `document.readyState !== 'complete'`)
					unsafeTrustedSubmitBypass = unsafeTrustedSubmitBypass ||
						!strings.Contains(expression, "curtain.trustedSubmit=true") ||
						!strings.Contains(expression, "finally{curtain.trustedSubmit=false}") ||
						!strings.Contains(expression, "Symbol.for('valorant.remote-captcha-curtain')")
					unsafeCaptchaInitWait = unsafeCaptchaInitWait ||
						!strings.Contains(expression, ".h-captcha,[data-hcaptcha-widget-id]") ||
						!strings.Contains(expression, "performance.now()") ||
						!strings.Contains(expression, "<2000")
				}
				unsafeSameEvaluation = unsafeSameEvaluation || (injectsCredentials && clicksSubmit)
				mu.Unlock()
				if injectsCredentials && !clicksSubmit {
					_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{
						"result": map[string]any{"type": "object", "value": map[string]any{"originOK": true, "filled": true}},
					}})
					continue
				}
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{
					"result": map[string]any{"type": "object", "value": map[string]any{"originOK": true, "submitted": true}},
				}})
				writeRiotLoginEvents(t, browser, command.SessionID, "get-login", "GET")
				writeRiotLoginEventsURL(t, browser, command.SessionID, "put-query-leak", "PUT", "https://authenticate.riotgames.com/?username=browser-user&password=browser-password")
				writeRiotLoginEventsStatus(t, browser, command.SessionID, "put-bad-status", "PUT", "https://authenticate.riotgames.com/api/v1/login", http.StatusBadGateway)
				writeRiotLoginEvents(t, browser, command.SessionID, "put-login", "PUT")
			case "Input.dispatchMouseEvent":
				eventType, _ := command.Params["type"].(string)
				mu.Lock()
				unexpectedTrustedInput = true
				mu.Unlock()
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{}})
				if eventType == "mouseReleased" {
					go func(sessionID string) {
						writeRiotLoginEvents(t, browser, sessionID, "get-login", "GET")
						writeRiotLoginEventsURL(t, browser, sessionID, "put-query-leak", "PUT", "https://authenticate.riotgames.com/?username=browser-user&password=browser-password")
						writeRiotLoginEventsStatus(t, browser, sessionID, "put-bad-status", "PUT", "https://authenticate.riotgames.com/api/v1/login", http.StatusBadGateway)
						writeRiotLoginEvents(t, browser, sessionID, "put-login", "PUT")
					}(command.SessionID)
				}
			case "Network.getResponseBody":
				requestID, _ := command.Params["requestId"].(string)
				body := `{"type":"success","success":{"login_token":"wrong-get-token"}}`
				if requestID == "put-login" {
					body = `{"type":"multifactor","multifactor":{"email":"b***@example.com","method":"email"}}`
				}
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{"body": body, "base64Encoded": false}})
			case "Network.getCookies":
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{"cookies": []map[string]any{
					{"name": "authenticator.sid", "value": "browser-session", "domain": "authenticate.riotgames.com", "path": "/", "secure": true, "httpOnly": true, "expires": -1},
				}}})
			default:
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "error": map[string]any{"message": "unexpected method"}})
			}
		}
	}()

	controller := &chromeBrowserController{profileDir: "private-profile", devToolsPipe: host}
	ownedClient, err := controller.chromeDevToolsClient()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := controller.RunRiotLogin(ctx, "browser-user", "browser-password")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotInjected := injectedCredentials
	gotUnsafeNativeSubmit := unsafeNativeSubmit
	gotUnsafeSubmitLookup := unsafeSubmitLookup
	gotUnsafeSameEvaluation := unsafeSameEvaluation
	gotUnsafeDOMClick := unsafeDOMClick
	gotUnsafeBeforeHydration := unsafeBeforeHydration
	gotUnsafeFillBeforeHydration := unsafeFillBeforeHydration
	gotUnsafeTrustedSubmitBypass := unsafeTrustedSubmitBypass
	gotUnsafeCaptchaInitWait := unsafeCaptchaInitWait
	gotDuplicatePasswordBinding := duplicatePasswordBinding
	gotCredentialEvaluations := credentialEvaluations
	gotSubmitEvaluations := submitEvaluations
	gotUnexpectedTrustedInput := unexpectedTrustedInput
	gotCurtainInstalled := curtainInstalled
	gotUnsafeCurtain := unsafeCurtain
	gotCredentialsBeforeCurtain := credentialsBeforeCurtain
	mu.Unlock()
	if !gotInjected {
		t.Fatal("official Riot form did not receive both credentials")
	}
	if !gotCurtainInstalled || gotUnsafeCurtain || gotCredentialsBeforeCurtain {
		t.Fatalf("document-start curtain installed=%t unsafe=%t credentials-before-curtain=%t", gotCurtainInstalled, gotUnsafeCurtain, gotCredentialsBeforeCurtain)
	}
	if gotUnsafeNativeSubmit {
		t.Fatal("credential injection may not invoke native form submission")
	}
	if gotUnsafeSubmitLookup {
		t.Fatal("credential injection must click only Riot's exact submit button within the credential form")
	}
	if gotUnsafeSameEvaluation {
		t.Fatal("Riot credentials and submit click must use separate evaluations so React can commit input state")
	}
	if gotUnsafeDOMClick {
		t.Fatal("Riot submit click must execute inside the guarded Runtime evaluation")
	}
	if gotUnsafeBeforeHydration {
		t.Fatal("Riot submit must wait for document load and React hydration before dispatching input")
	}
	if gotUnsafeFillBeforeHydration {
		t.Fatal("Riot credential fill must wait for document load and React hydration")
	}
	if gotUnsafeTrustedSubmitBypass {
		t.Fatal("Riot submit must open and close only the document curtain's one-shot trusted submit gate")
	}
	if gotUnsafeCaptchaInitWait {
		t.Fatal("Riot submit must wait for hCaptcha initialization before using its bounded no-captcha fallback")
	}
	if gotDuplicatePasswordBinding {
		t.Fatal("Riot submit expression must declare its password field exactly once")
	}
	if gotCredentialEvaluations != 1 || gotSubmitEvaluations != 1 {
		t.Fatalf("credential/submit evaluations = %d/%d, want 1/1", gotCredentialEvaluations, gotSubmitEvaluations)
	}
	if gotUnexpectedTrustedInput {
		t.Fatal("guarded submit unexpectedly used separate trusted input commands")
	}
	if !strings.Contains(string(result.ResponseBody), `"type":"multifactor"`) {
		t.Fatalf("response body = %s; GET response must not win", result.ResponseBody)
	}
	if result.UserAgent != "official-browser/1" || len(result.Cookies) != 1 || result.Cookies[0].Value != "browser-session" {
		t.Fatalf("browser result = %+v", result)
	}
	ownedClient.mu.Lock()
	ownedCalls := ownedClient.nextID
	ownedClient.mu.Unlock()
	if controller.devToolsClient != ownedClient || ownedCalls == 0 {
		t.Fatal("Riot login did not reuse the controller-owned DevTools client")
	}
}

func TestRiotBrowserStopsOnCredentialInjectionProtocolError(t *testing.T) {
	var evaluateCalls atomic.Int32
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	go func() {
		for {
			var command struct {
				ID        int64  `json:"id"`
				Method    string `json:"method"`
				SessionID string `json:"sessionId"`
			}
			if readErr := browser.ReadJSON(&command); readErr != nil {
				return
			}
			switch command.Method {
			case "Target.getTargets":
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{"targetInfos": []map[string]any{{
					"targetId": "riot-page", "type": "page", "url": "https://authenticate.riotgames.com/",
				}}}})
			case "Target.attachToTarget":
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{"sessionId": "riot-session"}})
			case "Page.addScriptToEvaluateOnNewDocument":
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{"identifier": "curtain-script"}})
			case "Runtime.evaluate":
				evaluateCalls.Add(1)
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "error": map[string]any{"message": "credential fields unavailable"}})
			default:
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{}})
			}
		}
	}()

	controller := &chromeBrowserController{profileDir: "private-profile", devToolsPipe: host}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := controller.RunRiotLogin(ctx, "browser-user", "browser-password")
	if err == nil || !strings.Contains(err.Error(), "credential fields unavailable") {
		t.Fatalf("RunRiotLogin error = %v", err)
	}
	if got := evaluateCalls.Load(); got != 1 {
		t.Fatalf("Runtime.evaluate calls = %d, want 1", got)
	}
	if elapsed := time.Since(started); elapsed >= 400*time.Millisecond {
		t.Fatalf("permanent protocol error retried until timeout: %s", elapsed)
	}
}

func TestRiotBrowserRetriesCredentialInjectionAcrossOneNavigationContext(t *testing.T) {
	var evaluateCalls atomic.Int32
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	go func() {
		for {
			var command struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := browser.ReadJSON(&command); err != nil {
				return
			}
			switch command.Method {
			case "Runtime.evaluate":
				call := evaluateCalls.Add(1)
				if call == 1 {
					_ = browser.WriteJSON(map[string]any{"id": command.ID, "error": map[string]any{
						"message": "Execution context was destroyed, most likely because of a navigation.",
					}})
					continue
				}
				expression, _ := command.Params["expression"].(string)
				value := map[string]any{"originOK": true, "filled": true}
				if strings.Contains(expression, "btn-signin-submit") {
					value = map[string]any{"originOK": true, "submitted": true}
				}
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{
					"result": map[string]any{"type": "object", "value": value},
				}})
			case "Input.dispatchMouseEvent":
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{}})
			default:
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "error": map[string]any{"message": "unexpected method"}})
			}
		}
	}()
	client := &chromeDevToolsClient{conn: host}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.submitRiotCredentials(ctx, "browser-user", "browser-password"); err != nil {
		t.Fatal(err)
	}
	if got := evaluateCalls.Load(); got != 3 {
		t.Fatalf("Runtime.evaluate calls = %d, want one navigation retry plus fill and submit", got)
	}
}

func TestRiotBrowserRetriesAllChromiumNavigationProtocolErrors(t *testing.T) {
	for _, message := range []string{
		"Cannot find default execution context",
		"Inspected target navigated or closed",
		"Not attached to an active page",
	} {
		t.Run(message, func(t *testing.T) {
			var evaluateCalls atomic.Int32
			host, browser := newTestChromeDevToolsPipes()
			t.Cleanup(func() {
				_ = host.Close()
				_ = browser.Close()
			})
			go serveCredentialSubmission(t, browser, &evaluateCalls, func(call int32, _ string) string {
				if call == 1 {
					return message
				}
				return ""
			})
			client := &chromeDevToolsClient{conn: host}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := client.submitRiotCredentials(ctx, "browser-user", "browser-password"); err != nil {
				t.Fatal(err)
			}
			if got := evaluateCalls.Load(); got != 3 {
				t.Fatalf("Runtime.evaluate calls = %d, want one navigation retry plus fill and submit", got)
			}
		})
	}
}

func TestRiotBrowserBoundsRepeatedNavigationContextErrors(t *testing.T) {
	var evaluateCalls atomic.Int32
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	go func() {
		for {
			var command struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if err := browser.ReadJSON(&command); err != nil {
				return
			}
			if command.Method == "Runtime.evaluate" {
				evaluateCalls.Add(1)
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "error": map[string]any{
					"message": "Execution context was destroyed, most likely because of a navigation.",
				}})
				continue
			}
			_ = browser.WriteJSON(map[string]any{"id": command.ID, "error": map[string]any{"message": "unexpected method"}})
		}
	}()
	client := &chromeDevToolsClient{conn: host}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	err := client.submitRiotCredentials(ctx, "browser-user", "browser-password")
	if err == nil || !strings.Contains(err.Error(), "Execution context was destroyed") {
		t.Fatalf("submit error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("navigation protocol errors retried until flow timeout: %s", elapsed)
	}
	// Nine calls is the independent contract: one initial attempt plus eight
	// navigation retries. Do not derive it from the production constant; this
	// assertion must catch an accidentally expanded credential-retention window.
	const want = int32(9)
	if got := evaluateCalls.Load(); got != want {
		t.Fatalf("Runtime.evaluate calls = %d, want exact bounded count %d", got, want)
	}
}

func TestRiotBrowserResetsNavigationRetryBudgetBeforeSubmit(t *testing.T) {
	var evaluateCalls atomic.Int32
	var fillErrors atomic.Int32
	var submitErrors atomic.Int32
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	go serveCredentialSubmission(t, browser, &evaluateCalls, func(_ int32, expression string) string {
		if strings.Contains(expression, "btn-signin-submit") {
			if submitErrors.Add(1) == 1 {
				return "Execution context was destroyed"
			}
			return ""
		}
		if fillErrors.Add(1) <= riotBrowserNavigationRetries {
			return "Execution context was destroyed"
		}
		return ""
	})
	client := &chromeDevToolsClient{conn: host}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.submitRiotCredentials(ctx, "browser-user", "browser-password"); err != nil {
		t.Fatal(err)
	}
	if got, want := fillErrors.Load(), int32(riotBrowserNavigationRetries+1); got != want {
		t.Fatalf("fill evaluations = %d, want %d", got, want)
	}
	if got := submitErrors.Load(); got != 2 {
		t.Fatalf("submit evaluations = %d, want independent retry then success", got)
	}
}

func TestRiotBrowserStopsNavigationRetryWhenCanceled(t *testing.T) {
	for _, phase := range []string{"fill", "submit"} {
		t.Run(phase, func(t *testing.T) {
			var evaluateCalls atomic.Int32
			errored := make(chan struct{})
			host, browser := newTestChromeDevToolsPipes()
			t.Cleanup(func() {
				_ = host.Close()
				_ = browser.Close()
			})
			go func() {
				for {
					var command struct {
						ID     int64          `json:"id"`
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					if err := browser.ReadJSON(&command); err != nil {
						return
					}
					if command.Method != "Runtime.evaluate" {
						_ = browser.WriteJSON(map[string]any{"id": command.ID, "error": map[string]any{"message": "unexpected method"}})
						continue
					}
					call := evaluateCalls.Add(1)
					expression, _ := command.Params["expression"].(string)
					atSubmit := strings.Contains(expression, "btn-signin-submit")
					if phase == "fill" || atSubmit {
						_ = browser.WriteJSON(map[string]any{"id": command.ID, "error": map[string]any{
							"message": "Execution context was destroyed",
						}})
						close(errored)
						return
					}
					if call != 1 {
						return
					}
					_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{
						"result": map[string]any{"type": "object", "value": map[string]any{"originOK": true, "filled": true}},
					}})
				}
			}()
			client := &chromeDevToolsClient{conn: host}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- client.submitRiotCredentials(ctx, "browser-user", "browser-password") }()
			select {
			case <-errored:
			case <-time.After(time.Second):
				t.Fatal("navigation error was not returned")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("submit error = %v, want context canceled", err)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("navigation retry ignored cancellation")
			}
			wantCalls := int32(1)
			if phase == "submit" {
				wantCalls = 2
			}
			if got := evaluateCalls.Load(); got != wantCalls {
				t.Fatalf("Runtime.evaluate calls after %s cancellation = %d, want %d", phase, got, wantCalls)
			}
		})
	}
}

func TestWaitRiotBrowserDiscoveryCancellationDoesNotWaitForTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	done := make(chan error, 1)
	neverFires := make(chan time.Time)
	go func() {
		done <- waitRiotBrowserDiscoveryEvent(ctx, neverFires, func() { close(entered) }, nil)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("retry wait did not reach its cancellation select")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("retry wait error = %v, want context canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("retry wait ignored cancellation while its timer was blocked")
	}
}

func TestWaitRiotBrowserDiscoveryTimerBranchPrefersCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	timerReady := make(chan time.Time, 1)
	timerReady <- time.Now()

	err := waitRiotBrowserDiscoveryEvent(ctx, timerReady, nil, cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("timer branch error = %v, want context canceled", err)
	}
}

func serveCredentialSubmission(t *testing.T, browser chromeDevToolsTransport, evaluateCalls *atomic.Int32, protocolError func(int32, string) string) {
	t.Helper()
	for {
		var command struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := browser.ReadJSON(&command); err != nil {
			return
		}
		switch command.Method {
		case "Runtime.evaluate":
			call := evaluateCalls.Add(1)
			expression, _ := command.Params["expression"].(string)
			if message := protocolError(call, expression); message != "" {
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "error": map[string]any{"message": message}})
				continue
			}
			value := map[string]any{"originOK": true, "filled": true}
			if strings.Contains(expression, "btn-signin-submit") {
				value = map[string]any{"originOK": true, "submitted": true}
			}
			_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{
				"result": map[string]any{"type": "object", "value": value},
			}})
		case "Input.dispatchMouseEvent":
			_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{}})
		default:
			_ = browser.WriteJSON(map[string]any{"id": command.ID, "error": map[string]any{"message": "unexpected method"}})
		}
	}
}

func TestRiotBrowserControllerCloseUnblocksPrivatePipeWhenOwnedProcessSurvives(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("a", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	closeCommandRead := make(chan struct{})
	go func() {
		defer close(closeCommandRead)
		var command map[string]any
		_ = browser.ReadJSON(&command)
	}()
	owner := trackCaptchaProcessOwnership(
		func(time.Duration) bool { return false },
		func(func(time.Duration) bool) error { return errors.New("owned process group survived") },
	)
	owner.stableGroup = true
	controller := &chromeBrowserController{
		devToolsPipe: host,
		processOwner: owner,
		profileRoot:  root,
		profileDir:   profileDir,
		exited:       make(chan struct{}),
		removeProfile: func(string, string) error {
			t.Fatal("live process profile must not be removed")
			return nil
		},
	}
	client, err := controller.chromeDevToolsClient()
	if err != nil {
		t.Fatal(err)
	}
	closeErr := controller.Close()
	if closeErr == nil || !captchaBrowserMayBeRunning(closeErr) {
		t.Fatalf("Close error = %v, want retained live-process failure", closeErr)
	}
	select {
	case <-closeCommandRead:
	case <-time.After(time.Second):
		t.Fatal("private Browser.close command was not consumed")
	}
	select {
	case <-client.done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("controller-owned private pipe reader remained blocked after process termination failed")
	}
}

func TestChromeBrowserControllerCloseUsesOwnedDevToolsClientSequence(t *testing.T) {
	root := t.TempDir()
	profileDir := filepath.Join(root, strings.Repeat("c", 32))
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	processExited := make(chan struct{})
	owner := trackCaptchaProcessOwnership(
		func(timeout time.Duration) bool {
			if timeout <= 0 {
				select {
				case <-processExited:
					return true
				default:
					return false
				}
			}
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case <-processExited:
				return true
			case <-timer.C:
				return false
			}
		},
		func(func(time.Duration) bool) error { return errors.New("unexpected process termination") },
	)
	controller := &chromeBrowserController{
		devToolsPipe: host,
		processOwner: owner,
		profileRoot:  root,
		profileDir:   profileDir,
		exited:       processExited,
		removeProfile: func(string, string) error {
			return nil
		},
	}
	client, err := controller.chromeDevToolsClient()
	if err != nil {
		t.Fatal(err)
	}
	versionDone := make(chan error, 1)
	go func() {
		versionDone <- client.Call(context.Background(), "Browser.getVersion", map[string]any{}, nil)
	}()
	version := nextRemoteCaptchaTestCommand(t, browser)
	if version.ID != 1 || version.Method != "Browser.getVersion" {
		t.Fatalf("first command=%+v", version)
	}
	replyRemoteCaptchaTestCommand(t, browser, version.ID)
	if err := <-versionDone; err != nil {
		t.Fatal(err)
	}

	closed := make(chan error, 1)
	go func() { closed <- controller.Close() }()
	closeCommand := nextRemoteCaptchaTestCommand(t, browser)
	if closeCommand.ID != 2 || closeCommand.Method != "Browser.close" {
		t.Fatalf("close command=%+v, want owned-client sequence ID 2", closeCommand)
	}
	replyRemoteCaptchaTestCommand(t, browser, closeCommand.ID)
	close(processExited)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("controller close did not finish")
	}
}

func writeRiotLoginEvents(t *testing.T, conn chromeDevToolsTransport, sessionID, requestID, method string) {
	t.Helper()
	writeRiotLoginEventsURL(t, conn, sessionID, requestID, method, "https://authenticate.riotgames.com/api/v1/login")
}

func writeRiotLoginEventsURL(t *testing.T, conn chromeDevToolsTransport, sessionID, requestID, method, requestURL string) {
	t.Helper()
	writeRiotLoginEventsStatus(t, conn, sessionID, requestID, method, requestURL, http.StatusOK)
}

func writeRiotLoginEventsStatus(t *testing.T, conn chromeDevToolsTransport, sessionID, requestID, method, requestURL string, status int) {
	t.Helper()
	for _, event := range []map[string]any{
		{"method": "Network.requestWillBeSent", "sessionId": sessionID, "params": map[string]any{"requestId": requestID, "request": map[string]any{"url": requestURL, "method": method}}},
		{"method": "Network.responseReceived", "sessionId": sessionID, "params": map[string]any{"requestId": requestID, "response": map[string]any{"url": requestURL, "status": status}}},
		{"method": "Network.loadingFinished", "sessionId": sessionID, "params": map[string]any{"requestId": requestID}},
	} {
		if err := conn.WriteJSON(event); err != nil {
			t.Errorf("write DevTools event: %v", err)
			return
		}
	}
}
