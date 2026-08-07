# Riot CAPTCHA Session Repair Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the bot-host password CAPTCHA produce a token accepted by Riot's current login session.

**Architecture:** Keep the existing authweb/riot separation. The browser page will match Riot's invisible hCaptcha invocation, while `PasswordClient` owns the correlated cookies and `sdksid` and emits Riot's current JSON request shape.

**Tech Stack:** Go 1.24, `net/http`, embedded JavaScript, hCaptcha JavaScript API.

## Global Constraints

- Do not bypass or automatically solve CAPTCHA.
- Keep credentials and session material memory-only.
- Preserve Discord modal password and MFA behavior.
- Keep Riot Mobile QR as the headless/server recommendation.

---

### Task 1: Lock the current browser contract with a failing test

**Files:**
- Modify: `internal/authweb/captcha_test.go`
- Test: `internal/authweb/captcha_test.go`

**Interfaces:**
- Consumes: `Server.Handler()` and the embedded `captchaWidgetHTML`.
- Produces: a regression test requiring invisible render plus `hcaptcha.execute(widgetId, {rqdata: ...})`.

- [ ] Add a page-response test that rejects `size: 'normal'` and `hcaptcha.setData`, and requires `size: 'invisible'` plus execute-time `rqdata`.
- [ ] Run `go test ./internal/authweb -run CaptchaWidget -count=1` and confirm it fails on the current checkbox implementation.

### Task 2: Lock the Riot request session with failing tests

**Files:**
- Modify: `internal/riot/password_test.go`
- Test: `internal/riot/password_test.go`

**Interfaces:**
- Consumes: `PasswordClient.BeginCaptcha`, `CompleteCaptcha`, and `SubmitMFA`.
- Produces: assertions for one stable `baggage: sdksid=...` value and the current JSON field locations.

- [ ] Extend the success test to capture POST/PUT headers and decode the PUT body.
- [ ] Assert a non-empty `sdksid` is identical across the CAPTCHA session.
- [ ] Assert `language` and `remember` are top-level and absent from `riot_identity`.
- [ ] Add an MFA assertion that the same `sdksid` reaches the MFA request.
- [ ] Run the focused tests and confirm the current implementation fails.

### Task 3: Implement the minimal protocol repair

**Files:**
- Modify: `internal/authweb/captcha.go`
- Modify: `internal/riot/password.go`

**Interfaces:**
- Consumes: Riot `captcha.hcaptcha.key/data` and the hCaptcha JavaScript API.
- Produces: correlated CAPTCHA/MFA HTTP requests and a callback token generated with the exact current `rqdata`.

- [ ] Render the widget with `size: 'invisible'`.
- [ ] Add a user-visible verify button and call `hcaptcha.execute(widgetId, {rqdata: rqdata})` from it.
- [ ] On retry, reset the widget and require another explicit user action with the replacement `rqdata`.
- [ ] Store a random `sdksid` in each `captchaSession`, propagate it into MFA challenges, and set the `baggage` header on all related requests.
- [ ] Move `language` and `remember` to the PUT body's top level.
- [ ] Run `gofmt` and the focused tests until green.

### Task 4: Verify, review, and deliver

**Files:**
- Modify only files already named above if review finds an issue.

**Interfaces:**
- Consumes: completed repair.
- Produces: a reviewed commit pushed to `origin/main` and a rebuilt local binary.

- [ ] Run `go test ./internal/authweb ./internal/riot -count=1`.
- [ ] Run `go test ./... -count=1`, `go test -race ./... -count=1`, `go vet ./...`, and build every `./cmd/...` package.
- [ ] Request an independent code review and fix all important findings.
- [ ] Re-run fresh verification after the final edit.
- [ ] Stage only the intended files, commit, push `main`, rebuild `bin/valorant-bot`, and report whether a manual CAPTCHA still needs to be solved after restart.

### Task 5: Repair browser/API session continuity after live rejection

**Files:**
- Modify: `internal/riot/password.go`
- Modify: `internal/authweb/server.go`
- Modify: `internal/authweb/captcha.go`
- Test: `internal/riot/password_test.go`
- Test: `internal/authweb/captcha_test.go`

**Interfaces:**
- Produces: `type CaptchaBrowserSession struct { UserAgent string; Cookies map[string]string }`.
- Changes: `BeginCaptcha` and `CompleteCaptcha` accept the browser session.
- Produces: `CaptchaChallenge.BrowserCookies` and `CaptchaRetryError.BrowserCookies` for HttpOnly synchronization.

- [ ] Write a Riot-client test in which the fake server sets literal cookies `authenticator.sid=s1` and `tdid=d1`; require begin and complete to use User-Agent `captcha-browser/1` and require a missing `tdid` to return `ErrCaptchaSession` without a PUT.
- [ ] Run `go test ./internal/riot -run 'TestPassword(BeginAndCompleteCaptcha|CompleteCaptchaRejectsDifferentBrowserSession)' -count=1` and observe RED.
- [ ] Implement browser User-Agent storage, defensive cookie export, exact completion-session validation, retry cookie export, and MFA User-Agent continuity.
- [ ] Write an authweb test that requires the challenge response to set `authenticator.sid` and `tdid` with `Secure` and `HttpOnly`, then returns those cookies on submission and asserts that the fake Riot client receives them with the same User-Agent.
- [ ] Run `go test ./internal/authweb -run 'TestCaptchaChallengeSyncsRiotBrowserSession' -count=1` and observe RED.
- [ ] Implement request-to-browser-session conversion, preserve Riot cookie attributes for initial and retry responses, require the expected Host/Origin, and isolate each auth state in an incognito Chrome profile.
- [ ] Run `gofmt`, `go test ./...`, `go test -race ./...`, `go vet ./...`, `git diff --check`, and `go build -o bin/valorant-bot ./cmd/bot`.
- [ ] Independently review the final diff, commit `Fix Riot captcha browser session`, and push `origin/main`.
