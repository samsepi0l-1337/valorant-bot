# Deploy

| Target       | One-shot script                          | Env template                 |
| ------------ | ---------------------------------------- | ---------------------------- |
| Local        | `./scripts/setup-local.sh`               | `env.local.example` → `.env` |
| Raspberry Pi | `./scripts/setup-pi.sh --host pi@…`      | `env.pi.example`             |
| Cloud / VPS  | `./scripts/setup-cloud.sh --host user@…` | `env.server.example`         |
| Docker       | `./scripts/setup-cloud.sh --docker`      | `.env.docker`                |

## CAPTCHA 브라우저 모드와 포트

`/auth` QR 로그인은 **인바운드 포트가 필요 없습니다**. 비밀번호 로그인은
`CAPTCHA_BROWSER_MODE`로 선택합니다.

| 모드 | 동작 | 필요한 조건 |
| --- | --- | --- |
| `local` (기본) | 봇 호스트의 GUI Chrome/Chromium 창 | 로그인된 데스크톱 세션 |
| `remote` | Pi/서버 Chromium 화면을 Discord 링크로 중계 | 공개 HTTPS `AUTH_BASE_URL`, WebSocket 프록시, Chromium, Xvfb |
| `disabled` | 비밀번호 CAPTCHA를 거절하고 QR 안내 | QR만 사용 |

`remote`에서만 `AUTH_PORT`가 공개 HTTPS 프록시/Cloudflare Tunnel 뒤에 있어야 합니다.
`AUTH_BASE_URL`은 절대 `https://` 주소여야 하며 userinfo·query·fragment를 넣지 마세요.
Cloudflare Tunnel은 뷰어 HTML, WebSocket 프레임, 검증된 입력만 전달합니다. Riot 페이지는
Tunnel 아래에 프록시·프레임·재작성되지 않고 Pi Chromium 안에서 Riot의 실제 HTTPS origin으로
열립니다. Windows/모바일 사용자는 다운로드·확장·인증서·localhost 리스너가 필요 없습니다.

`local` 비밀번호 로그인은 봇 호스트 GUI 흐름입니다:

1. The Discord user submits the ID/password modal, then the same Discord user
   clicks the CAPTCHA open/re-open button.
2. The bot opens Riot's official login page in GUI Chrome/Chromium on the
   **bot host**. Chrome uses Riot's real DNS and TLS. The bot controls only this
   owned login window through a private DevTools pipe that is never exposed on
   a TCP port.
3. After CAPTCHA completion, the bot closes that Chrome window. Only if Riot
   asks for MFA does Discord show an MFA button and modal for the code.

`local` 사용자는 아무 것도 설치하거나 localhost URL을 열 필요가 없습니다. Riot의 등록된
`http://localhost/redirect` is parsed as a returned token URI; neither the user
nor the bot opens a localhost callback server.

| Port               | Role                                      |
| ------------------ | ----------------------------------------- |
| Discord / Riot     | outbound only                             |
| `AUTH_PORT` (8787) | `/invite`, 원격 모드의 HTTPS 프록시 upstream |
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

원격 CAPTCHA Pi 설정(Cloudflare Tunnel 포함)은
[`pi-cloudflare-tunnel.md`](pi-cloudflare-tunnel.md)를 따르세요. 빠른 Tunnel URL은
재시작마다 바뀌므로 테스트용이며, 지속적 Discord 링크에는 이름 있는 Tunnel 또는 소유한
HTTPS 역방향 프록시를 사용해야 합니다.

Automated tests cover the local flow's state transitions and cleanup; they do
not demonstrate a successful login against a live Riot account.

## Files

- `install.sh` / `uninstall.sh` — systemd on Linux
- `valorant-bot.service` — unit file
- `remote-captcha.conf` — 원격 모드 전용 환경 파일 (설치 시 base env와 분리)
- `valorant-captcha-display.service` — private Xvfb display unit
- `nginx.example.conf` — optional reverse proxy
- `env.*.example` — environment templates
- `../scripts/pi-tunnel.sh` — Cloudflare quick tunnel (원격 CAPTCHA 테스트 또는 `/invite`)
- `pi-cloudflare-tunnel.md` — Pi Xvfb/Chromium 및 HTTPS Tunnel 운영 절차

See the root [README](../README.md).
