# Deploy

| Target       | One-shot script                          | Env template                 |
| ------------ | ---------------------------------------- | ---------------------------- |
| Local        | `./scripts/setup-local.sh`               | `env.local.example` → `.env` |
| Raspberry Pi | `./scripts/setup-pi.sh --host pi@…`      | `env.pi.example`             |
| Cloud / VPS  | `./scripts/setup-cloud.sh --host user@…` | `env.server.example`         |
| Docker       | `./scripts/setup-cloud.sh --docker`      | `.env.docker`                |

## Ports

`/auth` QR login needs **no inbound port**.

Password login is a bot-host GUI flow, not a public web flow:

1. The Discord user submits the ID/password modal, then the same Discord user
   clicks the CAPTCHA open/re-open button.
2. The bot opens GUI Chrome/Chromium on the **bot host**. It maps
   `authenticate.riotgames.com` to loopback TLS so hCaptcha tokens originate
   from Riot's authentication host.
3. After CAPTCHA completion, the bot closes that Chrome window. Only if Riot
   asks for MFA does Discord show an MFA button and modal for the code.

The Discord user installs nothing and never opens a localhost URL. Password
login needs no inbound port or public tunnel. `AUTH_BASE_URL`, reverse proxies,
and Cloudflare Tunnel are for `/invite` and optional helper pages only; do not
use a public/tunnel URL for CAPTCHA. A headless Pi, VPS, or Docker host cannot
use this GUI password flow and should use Riot Mobile QR instead.

| Port               | Role                                      |
| ------------------ | ----------------------------------------- |
| Discord / Riot     | outbound only                             |
| `AUTH_PORT` (8787) | `/invite` + optional local helper pages    |
| 80                 | unused                                    |

### Raspberry Pi and server authentication

```bash
# Password login requires a desktop session and Chrome/Chromium on this host.
sudo ./bin/valorant-bot
```

Use password login only when the Pi/server has a usable GUI session. For
headless installations, choose Riot Mobile QR in Discord. Real Arduino boards
are not supported deployment targets for this Go bot.

For a public `/invite` URL only, run `./scripts/pi-tunnel.sh` and set
`AUTH_BASE_URL` to the printed HTTPS URL. This does not provide CAPTCHA access.

Automated tests cover the local flow's state transitions and cleanup; they do
not demonstrate a successful login against a live Riot account.

## Files

- `install.sh` / `uninstall.sh` — systemd on Linux
- `valorant-bot.service` — unit file
- `nginx.example.conf` — optional reverse proxy
- `env.*.example` — environment templates
- `../scripts/pi-tunnel.sh` — optional Cloudflare quick tunnel for `/invite`

See the root [README](../README.md).
