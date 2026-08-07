# Password CAPTCHA and MFA Flow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task, with a task-scoped review after every task.

**Goal:** Make Discord password authentication start CAPTCHA only after the initiating user clicks the button, close and remove the bot-host Chrome session deterministically, and complete owner-bound Riot MFA without offering unusable retries.

**Architecture:** `authweb.Server` remains the authority for pending credentials, Riot CAPTCHA/MFA sessions, ownership, expiry, and browser resources. Discord handlers only render owner-bound controls and pass the current Discord user ID back to `authweb`. The Chrome launcher returns a controller for one state-specific process/profile; server cleanup closes it independently of page JavaScript.

**Tech Stack:** Go 1.22+, `discordgo`, `net/http`, `github.com/gorilla/websocket`, SQLite-backed existing store, Chrome/Chromium DevTools protocol, Go `testing` and race detector.

## Global Constraints

- The Discord user installs no helper, extension, certificate, hosts entry, or executable and never opens a localhost URL on their own device.
- CAPTCHA runs only in Chrome/Chromium on the bot host with `authenticate.riotgames.com` mapped to loopback; never expose the CAPTCHA through `AUTH_BASE_URL`, Cloudflare Tunnel, or another public hostname.
- Credential modal submission must not launch Chrome. Only the initiating Discord user's `로봇이 아닙니다` component interaction may launch or replace the browser.
- Every CAPTCHA and MFA state is bound to the initiating Discord user. Wrong-owner, expired, and consumed MFA operations must be rejected before any Riot request.
- CAPTCHA retry keeps the current Discord wait alive. A fresh Riot session preserves the existing cookie/User-Agent/challenge-continuity rules.
- CAPTCHA success closes the server-owned browser before the flow reports MFA or completion. Error, cancellation, expiry, and reopen also close the owned browser and remove its state-specific profile.
- Invalid Riot MFA codes remain retryable. Any error after Riot accepts/consumes an MFA code, including identity lookup, encryption, or persistence failure, is terminal and must not return a retry button for the consumed state.
- Username/password are erased from pending memory on transition to MFA, success, terminal failure, cancellation, and expiry. Credentials, codes, cookies, and tokens must never be logged or rendered.
- Keep Discord authentication controls ephemeral and acknowledge component interactions before browser or Riot work.
- Headless Raspberry Pi/server deployments use Riot Mobile QR. A real Arduino/ESP32 is not a supported runtime for this Go/SQLite/Chrome application.
- Use red-green TDD for every behavior change. Keep focused test output pristine and run the repository-wide test, race, vet, and build gates before push.

---

### Task 1: Make password CAPTCHA explicitly button-started

**Files:**

- Modify: `internal/authweb/captcha.go`
- Modify: `internal/authweb/captcha_test.go`
- Modify: `internal/bot/types.go`
- Modify: `internal/bot/handlers.go`
- Modify: `internal/bot/handlers_test.go`
- Modify: `internal/bot/discord_internal_test.go`

**Step 1: Write failing authweb tests**

Add tests proving that `BeginPasswordLogin` creates a live pending state but never calls the browser launcher, and that the first owner call to `LaunchPasswordCaptcha` calls it exactly once. Retain the wrong-owner regression and assert a wrong-owner click does not launch Chrome.

**Step 2: Run the focused tests and capture RED**

Run:

```bash
go test ./internal/authweb -run 'TestBeginPasswordLogin_DoesNotLaunchChrome|TestLaunchPasswordCaptcha'
```

The new no-auto-launch test must fail because `BeginPasswordLogin` currently starts `prepareCaptchaPage` asynchronously.

**Step 3: Remove automatic browser launch**

Change `BeginPasswordLogin` so it only validates and stores the owner-bound credential state, starts expiry, and returns the state. Remove `prepareCaptchaPage` and its goroutine. Update comments and interface documentation to say `BeginPasswordLogin` prepares a button-launched flow.

Keep `LaunchPasswordCaptcha` responsible for waiting on the loopback TLS listener and launching Chrome. Do not create the Riot CAPTCHA session until the exact browser requests the challenge endpoint.

**Step 4: Verify Discord button behavior**

Update handler tests so the credential modal response contains the CAPTCHA button and no browser launch occurs until the component handler receives the owner's click. Keep the existing acknowledgement-before-launch regression. Assert a rejected owner check renders the localized denial without starting Riot/browser work.

**Step 5: Run focused and package tests**

Run:

```bash
go test ./internal/authweb -run 'TestBeginPasswordLogin|TestLaunchPasswordCaptcha'
go test ./internal/bot -run 'TestHandlePasswordLogin|TestHandlePasswordCaptcha|TestCaptchaComponent'
go test ./internal/authweb ./internal/bot
```

**Step 6: Commit**

```bash
git add internal/authweb/captcha.go internal/authweb/captcha_test.go internal/bot/types.go internal/bot/handlers.go internal/bot/handlers_test.go internal/bot/discord_internal_test.go
git commit -m "Start Riot captcha from Discord button"
```

---

### Task 2: Own and clean up each Chrome CAPTCHA session

**Files:**

- Modify: `internal/authweb/server.go`
- Modify: `internal/authweb/captcha.go`
- Modify: `internal/authweb/captcha_launch.go`
- Create: `internal/authweb/captcha_process_unix.go`
- Create: `internal/authweb/captcha_process_windows.go`
- Modify: `internal/authweb/captcha_tls_test.go`
- Modify: `internal/authweb/captcha_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Step 1: Write failing lifecycle tests**

Introduce a test browser controller with an idempotent `Close() error`. Add tests proving:

- one owner launch stores one controller;
- reopen closes the existing controller before installing a replacement;
- CAPTCHA success closes the controller before `WaitPasswordLogin` returns MFA or completion;
- terminal CAPTCHA errors, wait cancellation, and expiry close it;
- cleanup erases username/password; and
- controller close removes its state profile even when graceful DevTools close is unavailable.

Use channels and event order assertions instead of sleeps wherever possible.

**Step 2: Run the focused tests and capture RED**

Run:

```bash
go test ./internal/authweb -run 'TestCaptchaBrowser|TestPassword.*Cleanup|TestLaunchPasswordCaptcha.*Replace'
```

The lifecycle tests must fail because the launcher currently returns only an error and cleanup owns no browser handle.

**Step 3: Add the browser-controller contract**

Replace `launchCaptchaBrowser func(string) error` with a launcher returning a per-state controller. The controller must expose an idempotent `Close() error`. Store the controller on the password flow or pending state without holding `Server.mu` while starting, waiting for, or closing an OS process.

`ensureCaptchaLaunched` must install a newly launched controller only if the state/flow is still live. If the state expires while launch is in progress, close the new controller immediately. Reopen must detach and close the old controller before launching the replacement, and a launch failure must leave the button usable.

**Step 4: Implement deterministic Chrome close**

Add these Chrome arguments:

```text
--remote-debugging-address=127.0.0.1
--remote-debugging-port=0
```

Read `DevToolsActivePort` from the state-specific `--user-data-dir`, connect only to its loopback WebSocket endpoint, and send the DevTools `Browser.close` command. Bound all discovery, dial, write, and exit waits. If graceful close is unavailable, terminate only the process group created for that launcher, wait for exit, and remove the state-specific profile directory with `os.RemoveAll` after validating it is the exact generated state directory.

Use OS-specific helpers: Unix starts a new process group and sends TERM followed by KILL if needed; Windows falls back to the owned process handle. Ensure `cmd.Wait()` is called exactly once and startup still reports immediate process failure.

**Step 5: Wire cleanup to every terminal path**

Close/detach the controller before emitting CAPTCHA success, MFA handoff, or terminal error. Also close it from `cleanupPasswordState`, wait cancellation, expiry, and explicit reopen. Keep page-level `window.close()` as a best-effort visual hint only.

Add a helper that overwrites pending `username` and `password` fields when the CAPTCHA leaves the solving state. Never log either value.

**Step 6: Run focused, race, and package tests**

Run:

```bash
go test ./internal/authweb -run 'TestCaptchaBrowser|TestPassword.*Cleanup|TestLaunchPasswordCaptcha'
go test -race ./internal/authweb
go test ./internal/authweb
```

**Step 7: Commit**

```bash
git add internal/authweb/server.go internal/authweb/captcha.go internal/authweb/captcha_launch.go internal/authweb/captcha_process_unix.go internal/authweb/captcha_process_windows.go internal/authweb/captcha_tls_test.go internal/authweb/captcha_test.go go.mod go.sum
git commit -m "Manage Riot captcha Chrome lifecycle"
```

---

### Task 3: Enforce MFA ownership and terminal persistence semantics

**Files:**

- Modify: `internal/authweb/server.go`
- Modify: `internal/authweb/captcha.go`
- Modify: `internal/authweb/captcha_test.go`
- Modify: `internal/bot/types.go`
- Modify: `internal/bot/handlers.go`
- Modify: `internal/bot/discord.go`
- Modify: `internal/bot/handlers_test.go`
- Modify: `internal/bot/discord_internal_test.go`
- Modify: `internal/i18n/i18n.go`
- Modify: `internal/i18n/i18n_test.go`

**Step 1: Write failing MFA security tests**

Add server tests that create an MFA state through CAPTCHA and prove:

- owner validation succeeds in memory;
- a wrong user cannot open the MFA modal;
- a wrong user cannot submit a code and `SubmitMFA` is not called;
- an expired/consumed state is rejected before Riot;
- an invalid code keeps the same state for another owner attempt;
- a successful code is consumed once under concurrent submissions; and
- persistence failure consumes the state and cannot be retried.

Add bot/Discord tests proving the current interaction user ID is passed for both MFA modal open and submit, denial is ephemeral/localized, invalid-code responses contain the retry button, and terminal failures contain no retry component.

**Step 2: Run focused tests and capture RED**

Run:

```bash
go test ./internal/authweb -run 'TestMFAOwner|TestMFAPersistence|TestMFASubmission'
go test ./internal/bot -run 'Test.*MFA'
```

The tests must fail because current MFA open/submit APIs do not take the Discord owner and all submission errors currently render retry controls.

**Step 3: Add owner-aware MFA APIs**

Add an exported sentinel `ErrMFAOwner`. Extend the bot/authweb contract with an in-memory `ValidatePasswordMFA(mfaState, discordUserID string) (hint string, err error)` call for the component-open path. Change completion to:

```go
CompletePasswordMFA(ctx context.Context, mfaState, discordUserID, code string) (displayName string, err error)
```

Both functions must compare the state owner before any Riot call. Keep submission serialization and live-state rechecks. The MFA-open Discord component may perform this quick in-memory check before responding with a modal; failures respond ephemerally instead of exposing the modal.

**Step 4: Distinguish retryable and terminal MFA failures**

Return retry controls only for Riot invalid-code errors while the owner-bound state remains live. Expired, wrong-owner, consumed, transport, identity, encryption, and persistence errors are terminal in the Discord response and clear the cached MFA hint.

After `SubmitMFA` succeeds, atomically consume the MFA state before identity resolution/persistence. A downstream error must remain terminal: never resurrect or reuse the Riot challenge and never render the old MFA button.

**Step 5: Finish credential lifetime cleanup**

When CAPTCHA creates `mfaPending`, move only the Riot challenge/session data and owner ID; blank username/password in the password pending record before publishing the MFA outcome. Verify terminal/success cleanup from Task 2 still blanks credentials and closes the browser before the Discord watcher observes completion.

**Step 6: Run focused, race, and package tests**

Run:

```bash
go test ./internal/authweb -run 'TestMFAOwner|TestMFAPersistence|TestMFASubmission|TestCaptchaChallengeAndSubmit_MFA'
go test ./internal/bot -run 'Test.*MFA'
go test -race ./internal/authweb ./internal/bot
go test ./internal/authweb ./internal/bot ./internal/i18n
```

**Step 7: Commit**

```bash
git add internal/authweb/server.go internal/authweb/captcha.go internal/authweb/captcha_test.go internal/bot/types.go internal/bot/handlers.go internal/bot/discord.go internal/bot/handlers_test.go internal/bot/discord_internal_test.go internal/i18n/i18n.go internal/i18n/i18n_test.go
git commit -m "Secure Riot MFA continuation"
```

---

### Task 4: Align operator guidance and run end-to-end gates

**Files:**

- Modify: `README.md`
- Modify: `deploy/README.md`
- Modify: relevant tests only if a documented command exposes an integration defect

**Step 1: Write documentation assertions if the project has doc tests**

If an existing test checks README/deploy text, extend it before editing. Otherwise treat command execution in the next steps as the executable acceptance check and do not add brittle string-only tests.

**Step 2: Update deployment guidance**

Document the exact password flow: Discord modal → owner clicks CAPTCHA button → Chrome opens on the bot host → Chrome closes after success → Discord MFA button/modal only when required. State clearly that the Discord user installs nothing, `AUTH_BASE_URL`/Cloudflare Tunnel is not used for CAPTCHA, a GUI Chrome session is required on the bot host, and headless Raspberry Pi/server setups should use Riot Mobile QR.

Remove or correct any statement that implies Riot requires literal localhost or that a real Arduino can run this application.

**Step 3: Format and inspect the diff**

Run:

```bash
gofmt -w internal/authweb internal/bot internal/i18n
git diff --check
```

**Step 4: Run all verification gates**

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/bot
```

All commands must exit zero with pristine output before final review. Do not claim real Riot authentication succeeded unless it was manually exercised with a live account; automated tests prove the state machine and cleanup behavior only.

**Step 5: Commit**

```bash
git add README.md deploy/README.md
git commit -m "Document bot-host Riot authentication"
```

**Step 6: Final review and push**

Generate a whole-change review package from commit `2f4da1a` through `HEAD`. Resolve every Critical/Important finding with one fix wave and scoped re-review. Re-run affected tests and the full verification gates after fixes.

When the final review is clean:

```bash
git status --short --branch
git push origin main
```

Confirm `main` matches `origin/main` and report the pushed commit range and actual verification commands.
