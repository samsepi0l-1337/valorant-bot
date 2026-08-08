# Remote CAPTCHA Browser Relay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow a Discord user on another computer to complete Riot CAPTCHA through a one-time HTTPS viewer while Chromium runs on the Raspberry Pi, without installing a user-side helper or exposing Riot credentials, cookies, or DevTools.

**Architecture:** The bot launches an owned GUI Chromium on Xvfb and keeps Riot authentication inside the existing private CDP pipe. A public auth server exposes only a one-time viewer shell, bearer redemption, and an authenticated WebSocket that relays bounded JPEG screencast frames plus validated pointer and wheel events. Local mode remains available; remote mode is explicit and requires an HTTPS public origin; disabled mode leaves QR authentication only.

**Tech Stack:** Go 1.23, `net/http`, Gorilla WebSocket, Chrome DevTools Protocol over private pipe, DiscordGo, SQLite, systemd, Xvfb, Chromium, Cloudflare Tunnel or an equivalent HTTPS reverse proxy.

## Global Constraints

- Never send Riot username, password, MFA code, cookies, authorization headers, CDP messages, or browser storage through the remote viewer.
- Never expose a DevTools TCP port, VNC, noVNC, or an unrestricted keyboard event channel.
- Store only SHA-256 digests of one-time bearer tokens and opaque viewer sessions in memory.
- Require exact configured public origin and host checks; do not trust arbitrary forwarded headers.
- Permit one active viewer per password flow, expire it after ten minutes, and allow at most sixty seconds of reconnect grace.
- Keep the existing Discord MFA modal and QR flow unchanged.
- Follow RED-GREEN-REFACTOR for every task and commit after each independently passing slice.
- Run race tests for every package that gains concurrent state.

---

### Task 1: Add an explicit CAPTCHA browser mode

**Files:**

- Create: `internal/netutil/captcha_mode.go`
- Create: `internal/netutil/captcha_mode_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `pkg/valorantbot/bot.go`
- Modify: `pkg/valorantbot/bot_scheduler_test.go`
- Modify: `cmd/bot/main.go`
- Modify: `internal/authweb/server.go`

- [ ] Write table tests for empty, `local`, `remote`, `disabled`, mixed-case, unsupported values, HTTP remote origins, HTTPS remote origins with paths, queries, fragments, or user-info.
- [ ] Confirm the tests fail because the mode parser and config fields do not exist.
- [ ] Add the normalized type and parser:

```go
package netutil

type CaptchaBrowserMode string

const (
	CaptchaBrowserLocal    CaptchaBrowserMode = "local"
	CaptchaBrowserRemote   CaptchaBrowserMode = "remote"
	CaptchaBrowserDisabled CaptchaBrowserMode = "disabled"
)

func NormalizeCaptchaBrowserMode(rawMode, authBaseURL string) (CaptchaBrowserMode, error)
```

- [ ] Make empty mode resolve to `local`; make `remote` require an absolute HTTPS origin with no user-info, query, fragment, or non-root path.
- [ ] Add `CAPTCHA_BROWSER_MODE` and `CAPTCHA_DISPLAY` to internal and public config types, preserving `:99` as the remote display default.
- [ ] Pass both fields from `cmd/bot` through `valorantbot.New` into `authweb.Deps` and store the normalized mode on `authweb.Server`.
- [ ] Make password CAPTCHA launch return an actionable error in `disabled` mode while QR remains available.
- [ ] Run:

```bash
gofmt -w internal/netutil/captcha_mode.go internal/netutil/captcha_mode_test.go internal/config/config.go internal/config/config_test.go pkg/valorantbot/bot.go pkg/valorantbot/bot_scheduler_test.go cmd/bot/main.go internal/authweb/server.go
go test ./internal/netutil ./internal/config ./pkg/valorantbot ./internal/authweb -count=1
go test -race ./internal/authweb -count=1
```

- [ ] Commit: `feat(auth): add captcha browser modes`

### Task 2: Create owner-bound remote viewer credentials

**Files:**

- Create: `internal/authweb/captcha_remote_state.go`
- Create: `internal/authweb/captcha_remote_state_test.go`
- Modify: `internal/authweb/captcha.go`
- Modify: `internal/authweb/server.go`
- Modify: `internal/authweb/shutdown.go`

- [ ] Write tests proving that `BeginPasswordLogin` in remote mode returns an HTTPS URL whose fragment contains a 32-byte random bearer, while local mode still returns no remote URL.
- [ ] Write tests proving only the SHA-256 bearer digest is retained, redemption is single-use, the Discord owner and password flow are bound, a second viewer cannot bind, cancellation removes all viewer state, and expiry removes state within the configured lifetime.
- [ ] Add deterministic clock and random-reader seams to avoid timing and entropy flakiness.
- [ ] Extend the password pending record with remote grant and opaque viewer-session metadata guarded by the server mutex.
- [ ] Generate URLs as `<AUTH_BASE_URL>/captcha/remote#<base64url-token>` and never place the token in a query, path, log field, error, or Discord custom ID.
- [ ] Add cleanup calls to password terminal transitions, owner cancellation, replacement flow creation, and server shutdown.
- [ ] Run:

```bash
gofmt -w internal/authweb/captcha_remote_state.go internal/authweb/captcha_remote_state_test.go internal/authweb/captcha.go internal/authweb/server.go internal/authweb/shutdown.go
go test ./internal/authweb -run 'TestRemoteCaptcha|TestBeginPasswordLogin' -count=20
go test -race ./internal/authweb -run 'TestRemoteCaptcha|TestBeginPasswordLogin' -count=10
```

- [ ] Commit: `feat(auth): issue one-time remote captcha grants`

### Task 3: Serve and redeem the hardened viewer shell

**Files:**

- Create: `internal/authweb/captcha_remote_http.go`
- Create: `internal/authweb/captcha_remote_http_test.go`
- Modify: `internal/authweb/server.go`

- [ ] Write HTTP tests for `GET /captcha/remote`, `POST /api/auth/captcha/remote/redeem`, and `POST /api/auth/captcha/remote/cancel`.
- [ ] Cover wrong host, wrong origin, missing origin, non-POST redemption, oversized body, malformed base64, reused bearer, expired bearer, wrong owner, and closed server.
- [ ] Assert redemption returns an opaque cookie with `Secure`, `HttpOnly`, `SameSite=Strict`, exact viewer path scope, and no bearer value.
- [ ] Assert HTML contains no remote bearer, clears `location.hash` before network activity, has no third-party scripts, and carries restrictive CSP, frame, cache, referrer, and content-type headers.
- [ ] Register public routes only in remote mode and keep legacy/private CAPTCHA endpoints off the public mux.
- [ ] Implement exact origin comparison against the configured `AUTH_BASE_URL` origin and exact request host comparison against its host.
- [ ] Limit redemption and cancel request bodies with `http.MaxBytesReader` and reject unknown JSON fields.
- [ ] Add a minimal viewer page with a canvas, connection status, cancel button, and pointer/wheel capture; do not add text or keyboard input.
- [ ] Run:

```bash
gofmt -w internal/authweb/captcha_remote_http.go internal/authweb/captcha_remote_http_test.go internal/authweb/server.go
go test ./internal/authweb -run 'TestRemoteCaptchaHTTP' -count=20
go test -race ./internal/authweb -run 'TestRemoteCaptchaHTTP' -count=10
```

- [ ] Commit: `feat(auth): serve secure captcha viewer shell`

### Task 4: Replace the synchronous CDP client with a dispatcher

**Files:**

- Create: `internal/authweb/captcha_devtools_client.go`
- Create: `internal/authweb/captcha_devtools_client_test.go`
- Modify: `internal/authweb/captcha_riot_browser.go`
- Modify: `internal/authweb/captcha_devtools_pipe.go`

- [ ] Write pipe-backed tests for concurrent calls, out-of-order replies, unsolicited events, malformed frames, duplicate response IDs, cancellation, pipe close, bounded event delivery, and shutdown without a readable peer.
- [ ] Record RED against the current single-reader client.
- [ ] Implement exactly one read goroutine that dispatches response IDs to pending calls and publishes events to a bounded channel.
- [ ] Add `Call(ctx, method, params, result)` and event subscription APIs; reject calls after close and unblock every waiter with a stable terminal error.
- [ ] Never log raw CDP payloads or returned values.
- [ ] Migrate the Riot credential submission and response adoption code to the dispatcher without changing its retry, origin, status, or navigation error contracts.
- [ ] Retain the private `--remote-debugging-pipe`; do not add a TCP fallback.
- [ ] Run:

```bash
gofmt -w internal/authweb/captcha_devtools_client.go internal/authweb/captcha_devtools_client_test.go internal/authweb/captcha_riot_browser.go internal/authweb/captcha_devtools_pipe.go
go test ./internal/authweb -run 'TestChromeDevTools|TestRiotBrowser' -count=20
go test -race ./internal/authweb -run 'TestChromeDevTools|TestRiotBrowser' -count=10
```

- [ ] Commit: `refactor(auth): multiplex private chrome devtools pipe`

### Task 5: Stream bounded frames and validate remote input

**Files:**

- Create: `internal/authweb/captcha_remote_stream.go`
- Create: `internal/authweb/captcha_remote_stream_test.go`
- Modify: `internal/authweb/captcha_launch.go`

- [ ] Write tests for `Page.startScreencast`, frame acknowledgement, newest-frame replacement, maximum decoded frame size, screencast stop, viewport bounds, event-rate bounds, malformed JSON, unsupported event types, and post-cancel input rejection.
- [ ] Implement JPEG screencasting at quality 80 and a fixed 1280 by 900 viewport.
- [ ] Keep a queue of one newest frame so slow viewers cannot grow memory.
- [ ] Reject base64 payloads whose decoded size would exceed 2 MiB before allocation.
- [ ] Accept only pointer move/down/up and wheel input; allow primary mouse button only; clamp coordinates to the negotiated viewport and validate finite numeric values.
- [ ] Enforce at most 60 input events per second per viewer with a bounded burst.
- [ ] Translate validated input to `Input.dispatchMouseEvent`; never expose a generic CDP forwarding endpoint.
- [ ] Couple stream lifetime to the password flow, Chrome process, viewer session, and server shutdown contexts.
- [ ] Run:

```bash
gofmt -w internal/authweb/captcha_remote_stream.go internal/authweb/captcha_remote_stream_test.go internal/authweb/captcha_launch.go
go test ./internal/authweb -run 'TestRemoteCaptchaStream' -count=20
go test -race ./internal/authweb -run 'TestRemoteCaptchaStream' -count=10
```

- [ ] Commit: `feat(auth): relay bounded captcha frames and input`

### Task 6: Add the authenticated one-viewer WebSocket

**Files:**

- Create: `internal/authweb/captcha_remote_ws.go`
- Create: `internal/authweb/captcha_remote_ws_test.go`
- Modify: `internal/authweb/captcha_remote_http.go`
- Modify: `internal/authweb/captcha.go`
- Modify: `internal/authweb/shutdown.go`

- [ ] Write WebSocket tests for a valid session, invalid cookie, wrong origin, wrong host, second concurrent viewer, reconnect inside grace, reconnect after grace, frame backpressure, oversized input, cancellation, browser exit, authentication completion, and shutdown drain.
- [ ] Use Gorilla WebSocket with strict origin and cookie validation before upgrade.
- [ ] Send JPEG bytes as binary frames and accept only the typed JSON mouse event schema.
- [ ] Start Chromium at most once for a remote flow, when the first authenticated viewer binds.
- [ ] Hold the viewer lease for sixty seconds after an unexpected disconnect; allow the same opaque session to reconnect and reject a different session.
- [ ] Set read and write size limits and deadlines, use ping/pong liveness, and make write ownership single-goroutine.
- [ ] Close the socket and stream immediately when the flow reaches success, MFA, terminal failure, owner cancellation, expiry, or server shutdown.
- [ ] Enroll WebSocket and stream workers in the auth server lifecycle wait group before publishing state.
- [ ] Run:

```bash
gofmt -w internal/authweb/captcha_remote_ws.go internal/authweb/captcha_remote_ws_test.go internal/authweb/captcha_remote_http.go internal/authweb/captcha.go internal/authweb/shutdown.go
go test ./internal/authweb -run 'TestRemoteCaptchaWebSocket' -count=20
go test -race ./internal/authweb -run 'TestRemoteCaptchaWebSocket' -count=10
```

- [ ] Commit: `feat(auth): connect one remote captcha viewer`

### Task 7: Update Discord password authentication controls

**Files:**

- Modify: `internal/bot/types.go`
- Modify: `internal/bot/handlers.go`
- Modify: `internal/bot/discord.go`
- Modify: `internal/bot/discord_internal_test.go`
- Modify: `internal/bot/handlers_test.go`
- Modify: `internal/i18n/i18n.go`
- Modify: `internal/i18n/i18n_test.go`

- [ ] Write handler tests for local, remote, and disabled modes before changing production code.
- [ ] In remote mode, assert the successful modal response contains a Discord link button to the fragment URL plus an owner-bound cancel button, and never contains the bearer in custom IDs or logs.
- [ ] Assert a definite Discord delivery failure cancels the password flow and grant; an ambiguous delivery failure preserves the flow; owner cancellation closes viewer and Chromium.
- [ ] Start the auth completion watcher only after Discord delivery is confirmed, while preserving the existing local launch and MFA continuation behavior.
- [ ] Keep all controls ephemeral and reject interactions from a different Discord owner.
- [ ] Add Korean and English copy explaining that the browser runs on the bot host and the link only relays the CAPTCHA screen.
- [ ] Run:

```bash
gofmt -w internal/bot/types.go internal/bot/handlers.go internal/bot/discord.go internal/bot/discord_internal_test.go internal/bot/handlers_test.go internal/i18n/i18n.go internal/i18n/i18n_test.go
go test ./internal/bot -run 'Test.*Password|Test.*Captcha' -count=20
go test -race ./internal/bot -run 'Test.*Password|Test.*Captcha' -count=10
```

- [ ] Commit: `feat(bot): deliver remote captcha viewer links`

### Task 8: Add opt-in Raspberry Pi Xvfb deployment

**Files:**

- Create: `deploy/valorant-captcha-display.service`
- Create: `deploy/remote-captcha.conf`
- Create: `internal/config/remote_captcha_assets_test.go`
- Modify: `deploy/install.sh`
- Modify: `deploy/uninstall.sh`
- Modify: `deploy/valorant-bot.service`
- Modify: `deploy/env.pi.example`
- Modify: `scripts/setup-pi.sh`
- Modify: `internal/authweb/captcha_launch.go`
- Modify: `internal/authweb/captcha_launch_test.go`

- [ ] Write asset tests that parse the service and environment files and assert `Xvfb :99`, `1280x900x24`, `-nolisten tcp`, service ordering, bot environment file loading, and no wildcard TCP listener.
- [ ] Write launch tests proving remote mode requires the configured display and local mode retains its desktop-user behavior.
- [ ] Add `valorant-captcha-display.service` running as the bot service user with `PrivateTmp=true`, `NoNewPrivileges=true`, strict filesystem protection, and restart-on-failure.
- [ ] Add `remote-captcha.conf` with `CAPTCHA_BROWSER_MODE=remote` and `CAPTCHA_DISPLAY=:99`.
- [ ] Add `scripts/setup-pi.sh --remote-captcha`; validate `Xvfb` and Chromium, print the exact apt install command when absent, and do not install packages silently.
- [ ] Make `deploy/install.sh --remote-captcha` install and enable the display unit, drop-in, and dependency ordering; make uninstall remove only files owned by this deployment.
- [ ] Ensure Chrome receives `DISPLAY=:99` through the existing sanitized allowlist without reintroducing bot secrets.
- [ ] Run:

```bash
gofmt -w internal/config/remote_captcha_assets_test.go internal/authweb/captcha_launch_test.go internal/authweb/captcha_launch.go
go test ./internal/config ./internal/authweb -run 'TestRemoteCaptchaAssets|Test.*RemoteDisplay' -count=10
go test -race ./internal/authweb -run 'Test.*RemoteDisplay' -count=5
bash -n deploy/install.sh deploy/uninstall.sh scripts/setup-pi.sh
```

- [ ] Commit: `feat(deploy): run remote captcha chrome on xvfb`

### Task 9: Document, verify, and publish

**Files:**

- Modify: `.env.example`
- Modify: `README.md`
- Modify: `deploy/README.md`
- Modify: `deploy/env.local.example`
- Modify: `deploy/env.pi.example`
- Modify: `deploy/env.server.example`
- Create: `deploy/pi-cloudflare-tunnel.md`

- [ ] Document that remote mode runs GUI Chromium on the Pi, streams only CAPTCHA frames and pointer input, requires an HTTPS public `AUTH_BASE_URL`, and does not require a Windows download or localhost listener.
- [ ] Document local and disabled modes, Discord link expiry, MFA modal behavior, Cloudflare Tunnel routing, and security boundaries.
- [ ] Add exact setup and rollback commands for the Xvfb unit and bot service.
- [ ] Run all native checks:

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./cmd/bot
git diff --check
```

- [ ] Run cross-platform compile checks without writing binaries into the repository:

```bash
tmpdir="$(mktemp -d)"
GOOS=linux GOARCH=arm64 go build -o "$tmpdir/valorant-bot-linux-arm64" ./cmd/bot
GOOS=linux GOARCH=arm GOARM=7 go build -o "$tmpdir/valorant-bot-linux-armv7" ./cmd/bot
GOOS=darwin GOARCH=amd64 go test -c -o "$tmpdir/authweb-darwin-amd64.test" ./internal/authweb
GOOS=windows GOARCH=amd64 go test -c -o "$tmpdir/authweb-windows-amd64.test.exe" ./internal/authweb
```

- [ ] Scan the diff for raw Discord tokens, Riot credentials, bearer values, CDP payload logging, generated databases, browser profiles, and build artifacts.
- [ ] On a configured HTTPS test origin, start the Pi/Mac host in remote mode, open the Discord-provided link on Windows, confirm the CAPTCHA renders and accepts pointer input, confirm the bot-host browser is not required on Windows, and confirm Riot MFA still arrives through the Discord modal.
- [ ] If live Riot testing cannot be performed because the public origin or account MFA is unavailable, record the exact unexecuted step without claiming it passed; automated tests remain mandatory.
- [ ] Commit documentation: `docs: explain remote captcha relay setup`
- [ ] Confirm `git status --short` contains no unintended files, then push the current branch to its configured upstream.

## Final Acceptance Checklist

- [ ] `CAPTCHA_BROWSER_MODE=local` preserves the current bot-host Chrome flow.
- [ ] `CAPTCHA_BROWSER_MODE=disabled` leaves QR authentication available and rejects password CAPTCHA clearly.
- [ ] `CAPTCHA_BROWSER_MODE=remote` requires HTTPS and opens no user-side localhost listener.
- [ ] The remote user installs nothing and receives an ephemeral one-time Discord link.
- [ ] Riot credentials, cookies, MFA, browser storage, and CDP remain on the bot host.
- [ ] Only bounded JPEG frames and validated pointer or wheel events cross the viewer channel.
- [ ] One viewer, expiry, reconnect grace, cancellation, completion, process exit, and shutdown are race-tested.
- [ ] Raspberry Pi deployment provides an opt-in Xvfb display with no TCP listener.
- [ ] Native tests, race tests, vet, host build, Linux ARM builds, and Darwin or Windows compile checks pass.
- [ ] The branch is clean and pushed only after all applicable gates complete.
