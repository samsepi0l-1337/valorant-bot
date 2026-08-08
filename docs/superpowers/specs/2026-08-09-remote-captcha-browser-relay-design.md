# Remote CAPTCHA Browser Relay Design

**Date:** 2026-08-09

## Goal

Allow a Discord user on Windows or mobile to complete the password-login
CAPTCHA while the bot runs on a headless Raspberry Pi, without installing a
helper, browser extension, VNC client, certificate, or executable on the
user's device.

The Riot page continues to run inside a bot-owned Chromium process at Riot's
real HTTPS origin. A short-lived web viewer relays only rendered frames and
validated pointer input between that process and the user's browser.

This design supersedes the bot-host-visible-window requirement in
`2026-08-08-password-captcha-mfa-flow-design.md`. It does not change the Riot
session, MFA, account persistence, or Riot Mobile QR decisions from that
design.

## User Flow

1. The user submits Riot credentials in the Discord password modal.
2. Discord returns an ephemeral `Open remote CAPTCHA` link and cancel control.
3. The link opens a generic viewer shell from the configured public HTTPS
   `AUTH_BASE_URL`. The bearer token stays in the URL fragment and is not sent
   in the initial HTTP request.
4. Viewer JavaScript redeems the fragment token over same-origin HTTPS, clears
   the fragment from browser history, and receives a secure viewer session.
5. The server starts one isolated Chromium process on the Raspberry Pi's Xvfb
   display and streams that page through its existing private DevTools pipe.
6. The user sees the Riot page in the viewer and completes hCaptcha by clicking
   the streamed image. Pointer and wheel input are applied to the Pi Chromium;
   Riot credentials and cookies are never sent to the viewer application.
7. CAPTCHA completion closes Chromium and the viewer. Successful login links
   the account, or Discord presents the existing MFA button and modal.
8. Rejection, cancellation, timeout, disconnect expiry, or bot shutdown closes
   the browser and deletes the state-specific profile and credentials.

## Modes and Configuration

Add an explicit `CAPTCHA_BROWSER_MODE` setting:

- `local`: preserve the current macOS/Linux desktop-window behavior.
- `remote`: enable the HTTPS viewer and Xvfb-backed Chromium behavior.
- `disabled`: expose Riot Mobile QR only and reject password CAPTCHA startup
  with a localized QR recommendation.

The default is `local` for backward compatibility. Raspberry Pi remote-CAPTCHA
templates set `remote` explicitly. Headless deployments that do not publish an
HTTPS viewer use `disabled`.

Remote mode requires:

- an absolute `https://` `AUTH_BASE_URL` with no user info, query, or fragment;
- `CAPTCHA_DISPLAY`, defaulting to `:99` in the Pi template;
- Chromium available to the service user;
- an Xvfb display owned by the `valorant` service user; and
- reverse-proxy WebSocket support for the existing `AUTH_PORT` listener.

The application fails remote-mode password login closed when these conditions
are unavailable. QR remains usable.

## Public HTTP Surface

The public auth mux adds only these remote-viewer routes:

- `GET /captcha/remote`: static viewer shell with no auth state in HTML.
- `POST /api/auth/captcha/remote/redeem`: consumes the fragment bearer and sets
  the viewer session cookie.
- `GET /api/auth/captcha/remote/ws`: upgrades an authenticated viewer session
  to its single WebSocket.
- `POST /api/auth/captcha/remote/cancel`: owner-session cancellation used by
  the viewer UI.

No route accepts a Riot username, password, MFA code, hCaptcha token, Riot
cookie, or login response. Those values remain inside the Discord interaction,
Go process, Riot client, and private Chrome transport.

The viewer HTML uses a restrictive Content Security Policy: same-origin script,
style, image, fetch, and WebSocket only; no frames, objects, external fonts,
third-party analytics, service worker, or persistent browser storage.

## Token and Viewer Session Model

`BeginPasswordLogin` creates two independent random values:

- the existing owner-bound password state; and
- a 32-byte remote-viewer bearer token.

Only the SHA-256 digest of the viewer bearer is retained in server memory. The
raw value appears only in the ephemeral Discord link fragment. It is never
logged, persisted, placed in a path/query, or returned after redemption.

Redemption rules:

- require remote mode and an exact request Host matching `AUTH_BASE_URL`;
- accept one JSON body with the raw fragment token under the existing request
  body limit;
- compare token digests in constant time;
- reject expired, canceled, terminal, already-bound, or unknown states;
- atomically bind the flow to one new 32-byte viewer-session identifier; and
- set a `Secure`, `HttpOnly`, `SameSite=Strict`, path-scoped session cookie.

The session cookie contains an opaque random identifier only. The server stores
its digest and flow association in memory. Cookie authentication may reconnect
the same browser during a 60-second disconnect grace period, but the bearer
cannot bind a second browser. A new device or cleared cookie requires a fresh
`/auth` flow.

The overall viewer lifetime is bounded by the existing password-flow TTL and a
10-minute maximum remote session, whichever expires first. No activity extends
the credential lifetime.

## Proxy and Origin Validation

Remote mode treats `AUTH_BASE_URL` as the only public origin. HTTP handlers
validate the request Host, and state-changing HTTP requests require an exact
`Origin` match. The WebSocket upgrader also requires that exact origin.

TLS termination may occur at Cloudflare Tunnel or another reverse proxy, so
the local Go request may be plain HTTP. The application does not trust arbitrary
forwarded headers to select an origin; it compares against the statically
configured HTTPS base URL and host. Direct requests with another Host or Origin
fail closed.

Cloudflare Tunnel transports viewer HTML, WebSocket frames, and input events
only. The Riot document is never served, framed, reverse-proxied, or rewritten
under the tunnel hostname. It stays in Pi Chromium at
`https://auth.riotgames.com` and `https://authenticate.riotgames.com`.

## Chromium and Virtual Display

Remote mode launches normal GUI Chromium on Xvfb rather than Chrome's headless
mode. This preserves the same browser page and User-Agent behavior as local
mode while allowing a Pi without a physical display to render it.

The dedicated display unit uses:

- display `:99` by default;
- a fixed 1280x900x24 screen;
- TCP listening disabled;
- the non-login `valorant` user and group; and
- restart-on-failure lifecycle separate from the bot process.

The browser still uses a unique hashed user-data directory, incognito mode,
real Riot DNS/TLS, a private `--remote-debugging-pipe`, sanitized environment,
owned process group, and existing bounded cleanup/reaper behavior. The profile
root remains under `/var/lib/valorant-bot`, the service's only writable path.

Remote mode never enables a DevTools TCP port or a VNC server. Xvfb itself is
not reachable over the network.

## DevTools Relay Boundary

The private DevTools connection gains a single-reader dispatcher so browser
login, response capture, screencast events, and input commands cannot race for
pipe frames. Command responses are correlated by ID; asynchronous events are
delivered through bounded internal subscriptions. Closing the flow terminates
all pending calls and subscriptions.

On viewer attachment the controller calls `Page.startScreencast` with JPEG
frames bounded to 1280x900 and a fixed quality suitable for image CAPTCHA.
Every `Page.screencastFrame` is acknowledged promptly. Only the newest frame is
retained in a size-one queue; a slow viewer drops old frames instead of growing
memory. Decoded and encoded frame sizes are capped before WebSocket delivery.

The viewer sends compact JSON input messages. The server accepts only:

- primary-button move, press, and release;
- vertical wheel scrolling; and
- viewport resize metadata required to map displayed coordinates.

Coordinates must be finite and inside the last acknowledged frame bounds.
Payload size, event rate, wheel magnitude, and outstanding input are bounded.
Arbitrary JavaScript, navigation, text injection, clipboard, file upload,
download, keyboard shortcuts, DevTools commands, and additional tabs are not
exposed to the viewer.

Riot credential injection remains the existing server-owned operation. The
viewer may see the username and masked password fields as rendered pixels, but
never receives their values in HTML, JSON, logs, or WebSocket messages.

## Discord Integration

Remote mode replaces the bot-host launch component with a Discord link button
whose URL is `<AUTH_BASE_URL>/captcha/remote#<bearer>`. The response remains
ephemeral. A separate owner-bound cancel component remains a Discord
interaction.

The password outcome watcher is enrolled only after Discord successfully
delivers the link. Browser launch begins only after successful bearer
redemption. Failure to deliver the link cancels the password flow immediately.

The existing local-mode component and watcher behavior remain unchanged. QR and
MFA components remain unchanged. When CAPTCHA reaches MFA or a terminal result,
the watcher edits the original ephemeral response, closes the viewer session,
and removes both link and cancel controls.

## Lifecycle and Error Handling

One password flow owns at most one browser generation and one active viewer
connection. Browser reopen/replacement generation fencing from local mode also
applies to remote mode.

- Redemption failure returns a generic expired/invalid page and does not reveal
  whether a Discord state exists.
- A second concurrent WebSocket receives conflict and cannot displace the first.
- A normal WebSocket drop starts a 60-second grace timer; reconnect cancels it.
- Explicit viewer or Discord cancellation terminates immediately.
- No reconnect before grace expiry cancels the flow and scrubs credentials.
- Riot CAPTCHA retry continues in the same owned browser and viewer.
- MFA, success, terminal Riot response, profile cleanup failure, server
  shutdown, and flow TTL are terminal for the viewer.
- Backpressure drops frames, never input validation or lifecycle events.
- Browser/Xvfb/pipe failure produces a localized retry-or-QR result in Discord.

All cleanup remains idempotent. Shutdown stops accepting redemptions and
WebSockets, closes active sockets, cancels CDP work, joins workers, terminates
owned browser groups, removes profiles, and only then closes persistent store
resources.

## Raspberry Pi Deployment

Add an opt-in `--remote-captcha` path to Pi setup. It:

1. verifies a supported Chromium executable and Xvfb;
2. installs the Xvfb systemd unit supplied by the project;
3. writes `CAPTCHA_BROWSER_MODE=remote` and `CAPTCHA_DISPLAY=:99`;
4. requires an HTTPS `AUTH_BASE_URL` before enabling password login;
5. enables the display unit before the bot unit; and
6. prints a health check and Cloudflare/reverse-proxy WebSocket checklist.

The installer must not silently install OS packages or enable remote CAPTCHA
for QR-only deployments. When dependencies are missing it prints the exact
package-manager command for the detected Debian-family Pi and exits before
changing service state.

The existing tunnel script remains an operator-started mechanism. Documentation
explains that a quick-tunnel URL changes across restarts and is suitable for
testing, while a stable named tunnel or owned reverse proxy is required for a
persistent Discord link origin.

## Testing Strategy

Implementation follows red-green TDD with deterministic seams and mutation-
sensitive assertions for:

1. Configuration parsing and remote-mode HTTPS fail-closed behavior.
2. Fragment bearer absence from the initial HTTP request and generated logs.
3. Token digest validation, one-browser binding, expiry, and replay rejection.
4. Exact Host, HTTP Origin, and WebSocket Origin enforcement.
5. Secure cookie attributes and reconnect grace behavior.
6. Link delivery success enrolling one watcher; delivery failure canceling the
   password flow and credentials.
7. Redemption starting exactly one browser; races cannot start two.
8. DevTools response/event routing with concurrent calls and cancellation.
9. Bounded screencast frames, newest-frame backpressure, and required ACKs.
10. Coordinate mapping, finite/bounds checks, rate limits, and payload limits.
11. Active-viewer conflict, reconnect, cancel, terminal result, and shutdown
    cleanup.
12. CAPTCHA-to-MFA and CAPTCHA-to-account completion with viewer closure before
    Discord continuation.
13. Local mode and QR regression coverage.
14. Systemd/Xvfb and Pi installer configuration rendering.

Required gates are focused normal/race tests, full `go test ./...`, full race,
`go vet ./...`, native build, Linux ARM64 bot build, Windows/Darwin compile
checks for unaffected modes, `git diff --check`, secret scan, process/profile
cleanup scan, and a real two-device manual test when an HTTPS relay is
available.

## Out of Scope

- Automated CAPTCHA solving or bypass.
- Streaming a full desktop or exposing VNC, RDP, DevTools TCP, or SSH.
- Loading, proxying, or framing Riot content under `AUTH_BASE_URL`.
- Multiple simultaneous viewers for one auth flow.
- Moving Riot credentials or MFA codes from Discord into the public viewer.
- Official Riot RSO migration or replacing the existing store session model.
- Supporting remote password CAPTCHA without a public HTTPS relay; those
  deployments use Riot Mobile QR.
