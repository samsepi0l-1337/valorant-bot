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
		duplicatePasswordBinding  bool
		credentialEvaluations     int
		submitEvaluations         int
		mousePressed              bool
		mouseReleased             bool
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
					unsafeDOMClick = unsafeDOMClick || clicksSubmit
					unsafeBeforeHydration = unsafeBeforeHydration ||
						!strings.Contains(expression, `document.readyState !== 'complete'`)
				}
				unsafeSameEvaluation = unsafeSameEvaluation || (injectsCredentials && clicksSubmit)
				mu.Unlock()
				if injectsCredentials && !clicksSubmit {
					_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{
						"result": map[string]any{"type": "object", "value": map[string]any{"filled": true}},
					}})
					continue
				}
				if locatesSubmit && !clicksSubmit {
					_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{
						"result": map[string]any{"type": "object", "value": map[string]any{"ready": true, "x": 410.5, "y": 722.5}},
					}})
					continue
				}
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": command.SessionID, "result": map[string]any{
					"result": map[string]any{"type": "object", "value": map[string]any{"submitted": true}},
				}})
				writeRiotLoginEvents(t, browser, command.SessionID, "get-login", "GET")
				writeRiotLoginEventsURL(t, browser, command.SessionID, "put-query-leak", "PUT", "https://authenticate.riotgames.com/?username=browser-user&password=browser-password")
				writeRiotLoginEventsStatus(t, browser, command.SessionID, "put-bad-status", "PUT", "https://authenticate.riotgames.com/api/v1/login", http.StatusBadGateway)
				writeRiotLoginEvents(t, browser, command.SessionID, "put-login", "PUT")
			case "Input.dispatchMouseEvent":
				eventType, _ := command.Params["type"].(string)
				x, xOK := command.Params["x"].(float64)
				y, yOK := command.Params["y"].(float64)
				mu.Lock()
				if xOK && yOK && x == 410.5 && y == 722.5 {
					mousePressed = mousePressed || eventType == "mousePressed"
					mouseReleased = mouseReleased || eventType == "mouseReleased"
				}
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
	gotDuplicatePasswordBinding := duplicatePasswordBinding
	gotCredentialEvaluations := credentialEvaluations
	gotSubmitEvaluations := submitEvaluations
	gotMousePressed := mousePressed
	gotMouseReleased := mouseReleased
	mu.Unlock()
	if !gotInjected {
		t.Fatal("official Riot form did not receive both credentials")
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
		t.Fatal("Riot submit must use a trusted Chrome input event, not HTMLElement.click")
	}
	if gotUnsafeBeforeHydration {
		t.Fatal("Riot submit must wait for document load and React hydration before dispatching input")
	}
	if gotUnsafeFillBeforeHydration {
		t.Fatal("Riot credential fill must wait for document load and React hydration")
	}
	if gotDuplicatePasswordBinding {
		t.Fatal("Riot submit expression must declare its password field exactly once")
	}
	if gotCredentialEvaluations != 1 || gotSubmitEvaluations != 1 {
		t.Fatalf("credential/submit evaluations = %d/%d, want 1/1", gotCredentialEvaluations, gotSubmitEvaluations)
	}
	if !gotMousePressed || !gotMouseReleased {
		t.Fatalf("trusted submit mouse events pressed/released = %t/%t, want true/true", gotMousePressed, gotMouseReleased)
	}
	if !strings.Contains(string(result.ResponseBody), `"type":"multifactor"`) {
		t.Fatalf("response body = %s; GET response must not win", result.ResponseBody)
	}
	if result.UserAgent != "official-browser/1" || len(result.Cookies) != 1 || result.Cookies[0].Value != "browser-session" {
		t.Fatalf("browser result = %+v", result)
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
				value := map[string]any{"filled": true}
				if strings.Contains(expression, "btn-signin-submit") {
					value = map[string]any{"ready": true, "x": 10.0, "y": 20.0}
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
						"result": map[string]any{"type": "object", "value": map[string]any{"filled": true}},
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
			value := map[string]any{"filled": true}
			if strings.Contains(expression, "btn-signin-submit") {
				value = map[string]any{"ready": true, "x": 10.0, "y": 20.0}
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
	blockedRead := make(chan error, 1)
	go func() {
		var response map[string]any
		blockedRead <- host.ReadJSON(&response)
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
	case err := <-blockedRead:
		if err == nil {
			t.Fatal("private pipe read unexpectedly succeeded")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("controller retained the private pipe after process termination failed")
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
