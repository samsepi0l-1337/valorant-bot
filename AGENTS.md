# AGENTS.md

## Learned User Preferences

- Build a Discord bot in the jettbot style: `/auth` offers Riot Mobile QR or
  Discord modal ID/password login, Riot credentials are bound to Discord user
  IDs, and one Discord user can manage multiple Riot accounts with daily store
  skins per `nickname#tag`.
- MVP must include automatic daily skin/store notification delivery, not only
  on-demand store checks; daily send time should be user-configurable (e.g.
  selectbox).
- Prefer Go for stability and concurrency; primary deployment target is
  Raspberry Pi (not Arduino), with local and server deploy paths as well.
- Prefer TDD with verification while building, and parallel non-overlapping
  subagent work when splitting implementation.
- Bot invite must use a Discord OAuth authorize URL so users pick the server; do
  not require configuring a Guild ID.
- Auth must need no user-installed helper and no user-side localhost: keep Riot
  Mobile QR, and Discord modal username/password then a bot-host Chrome「I'm not
  a robot」 window (tokens must be minted as `authenticate.riotgames.com`, not on
  trycloudflare.com); MFA uses a Discord modal only when Riot requires it.
  Headless Pi without a display should use QR (or a future captcha solver).
- Communicate in Korean for product and setup explanations unless asked
  otherwise; bot language settings should localize UI and skin display names.
- `/shop` and daily store messages should show resolved skin names and images
  (not UUIDs only); multi-account `/shop` uses Prev/Next so one page is one
  account with all 4 skins.
- Shop ownership/control errors (e.g. only the owner can use buttons) should be
  ephemeral embeds visible only to the interacting user.
- Wishlist add flow should help users pick from a skin list so they register
  accurate skin names.
- Linked accounts must display the correct Riot region/server (e.g. Asia/ap must
  not be labeled as kr).

## Learned Workspace Facts

- This repo is a Go Discord bot (`go run ./cmd/bot`) with SQLite storage,
  wishlist/guild alert settings, and a local auth HTTP server (default port
  `8787`).
- Discord setup uses `DISCORD_TOKEN` and `DISCORD_APP_ID`; invite URL is
  `https://discord.com/oauth2/authorize?client_id=<DISCORD_APP_ID>` (also logged
  and available at `/invite`).
- Slash commands register on guild join when the bot is invited via that OAuth
  link.
- `/auth` is a dual chooser: Riot Mobile QR (`internal/riot/qr.go`) and Discord
  modal password login; QR flow is POST
  `authenticate.riotgames.com/api/v1/login` with `qrcode:{}` → render
  `qrlogin.riotgames.com/riotmobile?...` → poll until `type:"success"` → POST
  `auth.riotgames.com/api/v1/login-token` (`persist_login:true`, yields `ssid`)
  → authorization for the token URI.
- The bot never binds port 80 and needs no inbound port for either `/auth`
  method. `AUTH_BASE_URL` / `AUTH_PORT` serve `/invite` and optional local
  helper pages. The Discord captcha re-open button invokes the bot host directly
  (it is not a localhost link on the user's device). Password captcha opens
  local Chrome with `--host-resolver-rules` against loopback TLS (port 443 when
  available, otherwise 8443) so hCaptcha tokens use host
  `authenticate.riotgames.com`. Tunnel-domain captcha tokens are rejected by
  Riot as `auth_failure`. `authcatcher` is not the primary auth path.
- QR and password logins store the `ssid` cookie (encrypted) so
  `ShopFetcher.resolveAccessToken` can `CookieReauth` for daily store checks.
- Multi-account `/shop` uses `BuildShopEmbeds` (one embed per account) plus
  Prev/Next buttons in `internal/bot/shop_nav.go` (`shop:page:{owner}:{page}`).
- Deploy docs and scripts under `deploy/` cover local, Raspberry Pi, and server
  setups; README setup guidance is Korean and OS-specific, and should describe
  only this project.
