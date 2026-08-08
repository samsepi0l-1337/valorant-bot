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
2. The bot opens Riot's official login page in GUI Chrome/Chromium on the
   **bot host**. Chrome uses Riot's real DNS and TLS. The bot controls only this
   owned login window through a private DevTools pipe that is never exposed on
   a TCP port.
3. After CAPTCHA completion, the bot closes that Chrome window. Only if Riot
   asks for MFA does Discord show an MFA button and modal for the code.

The Discord user installs nothing and never opens a localhost URL. Password
login needs no inbound port or public tunnel. Riot's registered
`http://localhost/redirect` is parsed as a returned token URI; neither the user
nor the bot opens a localhost callback server. `AUTH_BASE_URL`, reverse proxies,
and Cloudflare Tunnel are for `/invite` and optional helper pages only; do not
use a public/tunnel URL for CAPTCHA.

| Port               | Role                                      |
| ------------------ | ----------------------------------------- |
| Discord / Riot     | outbound only                             |
| `AUTH_PORT` (8787) | `/invite` + optional local helper pages    |
| 80                 | unused                                    |

### Raspberry Pi and server authentication

The GUI password flow is supported on macOS and Linux. Windows, headless Pi,
VPS, and Docker deployments should use Riot Mobile QR. The default `systemd`
deployment is deliberately a non-login `valorant` user
service. It has no desktop session or display configuration, so it cannot open
GUI Chrome even on a GUI-equipped Pi/server. Use Riot Mobile QR with the
default service; headless Pi, VPS, and Docker deployments support QR only.

To test or use password login on a GUI-equipped host, run a second, foreground
instance only from the account that is currently logged into the desktop. Do
not run the bot itself with `sudo` or `sudo -u`: those commands do not reliably
inherit the graphical session. Stop the service first so two instances never
connect to Discord at once. From that logged-in desktop user's terminal:

```bash
# Stop the installed service, but keep it enabled for the later restart.
sudo systemctl stop valorant-bot

# Copy the installed secret environment without printing it; only this desktop
# user can read the copy. Entering the user password for sudo is expected.
install -d -m 700 "$HOME/.config/valorant-bot"
sudo install -o "$USER" -g "$(id -gn)" -m 600 \
  /etc/valorant-bot/env "$HOME/.config/valorant-bot/desktop.env"

# The service database belongs to the service user. Use a private desktop-user
# database for this temporary foreground instance.
install -d -m 700 "$HOME/.local/state/valorant-bot"
set -a
. "$HOME/.config/valorant-bot/desktop.env"
set +a
DATABASE_PATH="$HOME/.local/state/valorant-bot/bot.db" \
  /usr/local/bin/valorant-bot
```

Run the last command from the active graphical terminal, where its display or
Wayland session is already inherited. Complete the password flow while this
foreground process runs; press `Ctrl-C` when finished. Then remove the
desktop-only secret copy and return to the normal QR-capable service:

```bash
rm -f "$HOME/.config/valorant-bot/desktop.env"
sudo systemctl start valorant-bot
```

The foreground instance intentionally uses a separate user-owned SQLite
database, so accounts linked there are not transferred to the system service.
Real Arduino boards are not supported deployment targets for this Go bot.

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
