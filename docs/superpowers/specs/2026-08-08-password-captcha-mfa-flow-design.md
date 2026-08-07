# Password CAPTCHA and MFA Flow Design

**Date:** 2026-08-08

## Goal

Provide a Jettbot-style Discord password login flow with a complete Riot MFA
continuation, while requiring no helper application, browser extension, hosts
file change, certificate installation, or other download on the Discord user's
mobile device or computer.

The user-visible sequence is:

1. Submit the Riot username and password in a Discord modal.
2. Click the Discord `로봇이 아닙니다` button.
3. The bot host opens an isolated Chrome CAPTCHA window.
4. Successful CAPTCHA completion closes that Chrome session.
5. If Riot requests MFA, Discord offers an `2차 인증 코드 입력` button that
   opens a code modal.
6. The account is linked only after CAPTCHA and any required MFA succeed.

## Constraints and Decisions

- The current password endpoint is not an official third-party Riot API. Riot's
  official third-party authentication mechanism is Riot Sign On (RSO), which
  redirects the user to Riot and requires an approved production application
  and RSO client. Until those credentials exist, this project retains its
  current password client and Riot Mobile QR fallback.
- Riot does not require a literal `localhost` CAPTCHA hostname. hCaptcha's own
  documentation warns that `localhost` and `127.0.0.1` are unsuitable supplied
  hostnames. The existing bot instead maps `authenticate.riotgames.com` to a
  loopback TLS listener inside a dedicated bot-host Chrome process.
- The `authenticate.riotgames.com` loopback mapping remains because the current
  login implementation depends on one continuous Riot challenge identity:
  hostname/origin, browser User-Agent, Riot/Cloudflare cookies, rqdata, and the
  one-time hCaptcha token.
- The CAPTCHA page is never exposed through `AUTH_BASE_URL`, Cloudflare Tunnel,
  or another public hostname. `AUTH_BASE_URL` continues to serve only optional
  helper pages such as `/invite`.
- The Discord user installs nothing. The bot host must already have
  Chrome/Chromium and a graphical desktop session for password CAPTCHA login.
  A headless Raspberry Pi/server uses Riot Mobile QR login.
- A real Arduino/ESP32 cannot run this Go/SQLite/Chrome application. Deployment
  requires a Linux-capable single-board computer such as a Raspberry Pi.

References:

- Riot RSO: <https://developer.riotgames.com/docs/faqs>
- hCaptcha local development: <https://docs.hcaptcha.com/#local-development>
- Jettbot public login guide:
  <https://support.teamfortuna.xyz/articles/9247feef-8261-4da6-8f1f-4dd3d09ef235>

## Authentication State Machine

| State | Trigger | Next state | User-visible result |
| --- | --- | --- | --- |
| Choosing | `/auth` | Credentials or QR | Password and QR choices |
| Credentials | Password button | Awaiting launch | Discord username/password modal |
| Awaiting launch | Valid modal submit | Solving CAPTCHA | `로봇이 아닙니다` button; Chrome is not opened yet |
| Solving CAPTCHA | Owner clicks button | MFA, Complete, Retry, or Failed | Isolated bot-host Chrome window |
| CAPTCHA retry | Riot returns a new challenge | Solving CAPTCHA | Same window renders the replacement challenge |
| MFA | Riot requests a second factor | Complete or MFA retry | Discord code-entry button and modal |
| Complete | Riot returns tokens and account persistence succeeds | Terminal | Linked Riot ID is shown |
| Failed/expired | Terminal error or timeout | Terminal | Actionable retry message and full cleanup |

Every CAPTCHA and MFA state is bound to the initiating Discord user. Concurrent
or duplicate submissions are serialized and only one successful completion may
consume a state.

## Discord Interaction Design

Submitting the credentials modal creates pending state but does not launch a
browser. Discord immediately replies with an ephemeral message containing the
owner-bound `로봇이 아닙니다` component.

The component interaction is acknowledged before browser startup. Clicking it
launches Chrome on the bot host. Repeated clicks do not accumulate Chrome
sessions: a live session is focused/reused where possible, or explicitly closed
before a replacement is started.

Discord cannot open a modal asynchronously after a browser HTTP callback. When
CAPTCHA succeeds and Riot asks for MFA, the original ephemeral message is
edited to show an `2차 인증 코드 입력` button. Clicking that component is the
new Discord interaction that opens the MFA modal.

MFA methods are represented as follows:

- Email: six-digit code, including Riot's masked email hint when available.
- Authenticator application: six- to eight-digit one-time code.
- Riot Mobile/code-compatible challenge: code modal when Riot returns a code
  method; QR remains the fallback for approval-only mobile challenges.

An invalid MFA code keeps the live state and presents the code-entry button
again. Expired, already-consumed, or wrong-owner states are rejected without
calling Riot.

## Browser Lifecycle

The browser launcher returns a per-auth-state controller instead of only an
`error`. The controller owns the dedicated Chrome profile and provides an
idempotent close operation.

Chrome continues to use:

- a unique hashed `--user-data-dir` for each auth state;
- incognito mode;
- `MAP authenticate.riotgames.com 127.0.0.1`;
- the existing local TLS certificate exception flags; and
- loopback-only Chrome DevTools control using an ephemeral port.

The controller discovers the isolated browser's DevTools endpoint from its
profile and sends `Browser.close`. If graceful close is unavailable, it
terminates only the process group created for that auth state. The controller
then waits for exit and removes the state-specific profile directory. The
DevTools listener is bound to loopback and is never included in Discord output
or public HTTP routing.

The server closes the browser on:

- CAPTCHA success before handing off to MFA;
- CAPTCHA success without MFA;
- terminal Riot error;
- Discord wait cancellation;
- auth-state expiry; and
- explicit replacement/reopen.

The page-level `window.close()` remains a fast user-interface hint, but server
cleanup is authoritative and does not rely on browser JavaScript being allowed
to close a command-line-opened window.

## Riot Session and MFA Continuity

The existing Riot challenge session remains the source of truth. The first
request from the exact CAPTCHA browser supplies the User-Agent. Discovery and
authenticate response cookies are filtered by domain/path/security scope,
synchronized to the browser, and reused by the CAPTCHA completion request.

When Riot returns an MFA challenge, its cookie jar, sdk session identifier,
User-Agent, method, and masked email hint move into a short-lived MFA state.
Password CAPTCHA state and Chrome resources are removed, but the MFA challenge
is retained until success, expiry, or cancellation.

Successful MFA token exchange is not considered complete until identity
resolution, session encryption, and SQLite account upsert all succeed. A final
persistence failure produces a terminal error rather than presenting a retry
button for an already-consumed Riot MFA state.

## Security and Privacy

- Credentials and MFA codes are never logged.
- Username/password exist only in the in-memory pending state and are erased on
  transition to MFA, success, failure, cancellation, or expiry.
- Discord responses containing auth controls remain ephemeral.
- CAPTCHA and MFA component handlers validate the initiating Discord user.
- Raw Riot/hCaptcha tokens, session cookies, and internal state values are not
  rendered in Discord messages or normal logs.
- Persisted Riot session material remains encrypted using `BOT_SECRET`.
- No public endpoint accepts Riot credentials.

## Error Handling

- CAPTCHA rejection with a fresh Riot challenge rerenders in the same Chrome
  session without finishing the Discord wait.
- Lost CAPTCHA responses reload the current authoritative challenge version.
- Session identity mismatch cancels the old Riot session, clears browser
  cookies, and starts a fresh challenge within the same Discord state.
- Chrome launch failure keeps the Discord component actionable and reports an
  installation/display error for the bot host.
- CAPTCHA and MFA expiry remove credentials, Riot session state, browser
  processes, temporary profiles, and owner-bound interaction state.
- Discord interactions are acknowledged before Chrome or Riot network work to
  avoid the three-second interaction timeout.

## Test Strategy

The implementation follows red-green TDD and adds regression coverage for:

1. Credentials modal submission does not launch Chrome.
2. Only the initiating Discord user can launch or reopen CAPTCHA.
3. One owner click creates one browser controller.
4. Reopen closes/replaces an existing controller without leaking a process or
   profile.
5. CAPTCHA success closes Chrome before MFA handoff or account completion.
6. Terminal error, cancellation, and expiry close Chrome and clear credentials.
7. CAPTCHA retry keeps the same state/browser and does not notify completion.
8. MFA ownership is enforced before Riot submission.
9. Invalid MFA codes remain retryable; concurrent submits reach Riot once.
10. Account persistence occurs only after successful MFA and is one-time.
11. A persistence failure does not offer a consumed MFA state for retry.
12. Korean and English button/modal messages describe the correct stage.

Focused package tests, race tests, full repository tests, `go vet`, and a native
build are required before the implementation is committed and pushed.

## Out of Scope

- Reverse-engineering private Jettbot implementation details.
- CAPTCHA solving services or automated CAPTCHA bypass.
- Requiring a Discord user-side helper, extension, custom DNS, hosts entry,
  certificate, or downloaded executable.
- Publicly proxying a Riot CAPTCHA under `trycloudflare.com` or another custom
  hostname.
- Official RSO integration before Riot issues an approved RSO client.
