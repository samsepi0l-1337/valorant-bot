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
