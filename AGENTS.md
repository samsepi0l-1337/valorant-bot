## Learned User Preferences

- Build a Discord bot in the jettbot style: `/auth` shows an ephemeral auth link, Riot credentials are bound to Discord user IDs, and one Discord user can manage multiple Riot accounts with daily store skins per `nickname#tag`.
- MVP must include automatic daily skin/store notification delivery, not only on-demand store checks; daily send time should be user-configurable (e.g. selectbox).
- Prefer Go for stability and concurrency; primary deployment target is Raspberry Pi (not Arduino), with local and server deploy paths as well.
- Prefer TDD with verification while building, and parallel non-overlapping subagent work when splitting implementation.
- Bot invite must use a Discord OAuth authorize URL so users pick the server; do not require configuring a Guild ID.
- Riot login must be fully automatic: `/auth` shows a QR code, the user approves it in Riot Mobile, and tokens are saved with the Discord message marked complete — no URL paste, no browser, no user-installed helper.
- Communicate in Korean for product and setup explanations unless asked otherwise; bot language settings should localize UI and skin display names.
- `/shop` and daily store messages should show resolved skin names and images (not UUIDs only), with embeds compact enough that about two accounts fit on one Discord screen.
- Wishlist add flow should help users pick from a skin list so they register accurate skin names.

## Learned Workspace Facts

- This repo is a Go Discord bot (`go run ./cmd/bot`) with SQLite storage, wishlist/guild alert settings, and a local auth HTTP server (default port `8787`).
- Discord setup uses `DISCORD_TOKEN` and `DISCORD_APP_ID`; invite URL is `https://discord.com/oauth2/authorize?client_id=<DISCORD_APP_ID>` (also logged and available at `/invite`).
- Slash commands register on guild join when the bot is invited via that OAuth link.
- `/auth` is a Riot Mobile QR login (`internal/riot/qr.go`): POST `authenticate.riotgames.com/api/v1/login` with `qrcode:{}` → render `qrlogin.riotgames.com/riotmobile?cluster&suuid&timestamp` → GET-poll the same login endpoint until `type:"success"` → POST `auth.riotgames.com/api/v1/login-token` (`persist_login:true`, yields `ssid`) → POST `/api/v1/authorization` for the token URI. Users must have the Riot Mobile app.
- The bot never binds port 80 and needs no inbound port for `/auth`; `AUTH_BASE_URL` / `AUTH_PORT` only serve `/invite` and the browser fallback login page (`authcatcher` remains as that fallback only).
- QR logins store the `ssid` cookie (encrypted) so `ShopFetcher.resolveAccessToken` can `CookieReauth` for daily store checks; the browser fallback still stores `access_token=...`.
- Deploy docs and scripts under `deploy/` cover local, Raspberry Pi, and server setups; README setup guidance is Korean and OS-specific.
