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
