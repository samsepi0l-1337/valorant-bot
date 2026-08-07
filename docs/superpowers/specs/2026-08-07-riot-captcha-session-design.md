# Riot CAPTCHA Session Repair Design

## Problem

The password `/auth` flow renders Riot's enterprise hCaptcha as a normal
checkbox and attaches `rqdata` later with `setData`. Riot's current
`authenticate.riotgames.com` UI instead renders an invisible widget and passes
the current `rqdata` directly to `hcaptcha.execute`. Tokens from the custom
flow are therefore issued but can be rejected by Riot as `invalid_request`.

The Go client also differs from Riot's current request shape: `language` and
`remember` are nested under `riot_identity`, and it does not preserve the
Riot SDK `sdksid` baggage value across the login session.

## Considered approaches

1. **Mirror Riot's current hCaptcha and login request protocol (chosen).** Keep
   the existing Discord modal and bot-host Chrome UX, but use an invisible
   hCaptcha widget, execute it with the challenge's `rqdata`, use Riot's
   top-level request fields, and preserve one `sdksid` across begin, CAPTCHA,
   and MFA requests. This is the smallest compatible repair.
2. **Automate Riot's full hosted login page.** This would keep every browser
   detail identical to Riot, but needs Chrome DevTools automation and becomes
   fragile whenever Riot changes its DOM.
3. **Remove password login and require Riot Mobile QR.** This is the safest
   server/headless deployment path, but does not repair the requested password
   flow.

## Data flow

1. Discord collects the Riot ID and password as it does today.
2. The bot obtains one Riot CAPTCHA challenge and retains its cookies and one
   generated `sdksid` only in memory.
3. Bot-host Chrome opens the local TLS page at
   `https://authenticate.riotgames.com`.
4. The page renders an invisible hCaptcha widget. A user action calls
   `hcaptcha.execute(widgetID, {rqdata: currentRQData})`.
5. The callback token is submitted once with the challenge version. Go sends
   it in Riot's current top-level login request shape and reuses the session's
   cookies and `sdksid`.
6. A fresh challenge replaces the widget data without ending the Discord
   wait. Success continues to MFA or account linking as before.

## Error handling and security

- Credentials and Riot cookies remain memory-only and are deleted on success,
  cancellation, or expiry.
- The page continues to reject any origin other than
  `authenticate.riotgames.com`.
- Retry responses keep the same session only when Riot returns a new
  challenge; terminal rejection tells the user to restart `/auth`.
- No CAPTCHA bypass or third-party solver is introduced.

## Tests

- Assert the generated page uses invisible rendering and executes with the
  current `rqdata`, without the old checkbox/setData sequence.
- Assert begin, CAPTCHA completion, and MFA reuse one non-empty `sdksid`.
- Assert the CAPTCHA PUT body has top-level `language`/`remember` and does not
  duplicate them under `riot_identity`.
- Run focused tests, the full suite, the race detector, `go vet`, and all
  command builds.

## Revision: browser session continuity

The protocol-shape repair above did not stop Riot's live `invalid_request`.
Inspection of the running CAPTCHA Chrome profile found only hCaptcha cookies;
Riot's `authenticator.sid`, `tdid`, and `__cflb` remained exclusively in the
Go client. The token was therefore created by a different HTTP identity from
the one that submitted it.

The repaired boundary passes an explicit browser session containing the
Chrome User-Agent and Riot cookie values. `BeginCaptcha` uses the browser
User-Agent and returns Riot's response cookies without exposing them in JSON.
The local HTTPS handler preserves Riot's cookie attributes (including Domain,
Path, Secure, HttpOnly, SameSite, and expiry) when it writes them for the
mapped `authenticate.riotgames.com` origin. Chrome returns those cookies with
the solved token, and `CompleteCaptcha` rejects a changed User-Agent or any
missing, extra, or mismatched allowlisted Riot cookie before contacting Riot.
Retry responses preserve Riot's cookie-deletion attributes as browser
tombstones. A replacement session explicitly clears retained browser cookies
before applying its new canonical cookie set, and MFA retains the browser
User-Agent with its server-side cookie session.

The Riot session is created only after Chrome makes the first challenge
request, so its User-Agent is known before the initial Riot call. Each auth
state uses an isolated incognito Chrome profile to prevent concurrent Discord
users from sharing Riot cookies. The local TLS endpoints require the expected
Host and same-origin Origin before accepting a CAPTCHA submission.
If browser/session validation fails, the page clears its isolated Riot cookie
jar and automatically obtains a new challenge instead of reusing the rejected
token.

Credentials remain only in Go memory. Cookie values, credentials, and CAPTCHA
tokens must never be logged. QR remains the preferred headless/server flow.
