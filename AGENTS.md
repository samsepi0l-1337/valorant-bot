## Learned User Preferences

- Build a Discord bot in the jettbot style: `/auth` shows an ephemeral auth link, Riot credentials are bound to Discord user IDs, and one Discord user can manage multiple Riot accounts with daily store skins per `nickname#tag`.
- MVP must include automatic daily skin/store notification delivery, not only on-demand store checks.
- Prefer Go for stability and concurrency; primary deployment target is Raspberry Pi (not Arduino).
- Prefer TDD with verification while building, and parallel non-overlapping subagent work when splitting implementation.
- Bot invite must use a Discord OAuth authorize URL so users pick the server; do not require configuring a Guild ID.
- Riot login should open from a button/URL the program provides, then persist tokens from the post-login redirect URL automatically (no manual token paste as the main path).
- Communicate in Korean for product and setup explanations unless asked otherwise.

## Learned Workspace Facts

- This repo is a Go Discord bot (`go run ./cmd/bot`) with SQLite storage, wishlist/guild alert settings, and a local auth HTTP server (default port `8787`).
- Discord setup uses `DISCORD_TOKEN` and `DISCORD_APP_ID`; invite URL is `https://discord.com/oauth2/authorize?client_id=<DISCORD_APP_ID>` (also logged and available at `/invite`).
- Slash commands register on guild join when the bot is invited via that OAuth link.
- For Raspberry Pi / LAN use, `AUTH_BASE_URL` must be a browser-reachable LAN URL; the auth server binds `0.0.0.0`.
- Riot auth completion arrives as a `playvalorant.com/.../opt_in/#access_token=...` hash redirect; the bot is expected to capture and store that redirect fragment.
