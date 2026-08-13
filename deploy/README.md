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
| `remote` | Pi/서버 Chromium 화면을 Discord 링크로 중계 | 인터넷: 공개 HTTPS `AUTH_BASE_URL` + WebSocket 프록시. LAN: 사설/로컬 HTTP + `AUTH_BIND_ADDRESS=0.0.0.0` 또는 LAN IP. Chromium, (Pi는) Xvfb |
| `disabled` | 비밀번호 CAPTCHA를 거절하고 QR 안내 | QR만 사용 |

인터넷 `remote`는 `AUTH_PORT`를 공개 HTTPS 프록시/Cloudflare Tunnel 뒤에 둡니다.
같은 LAN의 다른 PC만 쓰면 Tunnel 없이 사설/로컬 `http://` origin이 허용됩니다.
절차는 [`lan-remote-captcha.md`](lan-remote-captcha.md)입니다. 공개 HTTP 호스트는 거부됩니다.
`AUTH_BASE_URL`은 host가 비어 있지 않은 단 하나의 절대 origin이어야 합니다.
userinfo·query·fragment·공백·제어문자와 `/` 외의 path를 넣지 마세요. 설치 스크립트는
서비스·사용자·파일을 변경하기 전에 이 형식을 검사하고, 런타임에서는 애플리케이션의
canonicalizer가 다시 권위 있게 검증합니다.
`setup-pi.sh --host`는 build/scp 전에 sudo 암호 입력용 원격 TTY를 유지한 일회성 Python
validator로 대상의 기존 `/etc/valorant-bot/env`를 읽습니다. 검사기는 대상에 설치되지 않으며,
기존 origin이 caller 값과 정확히 다르면 어느 값도 출력하지 않고 중단합니다.
hostname을 권장하며 HTTPS IP 주소는 해당 IP에 유효한 TLS 인증서가 있을 때만 사용합니다.
Cloudflare Tunnel은 뷰어 HTML, WebSocket 프레임, 검증된 입력만 전달합니다. Riot 페이지는
Tunnel 아래에 프록시·프레임·재작성되지 않고 Pi Chromium 안에서 Riot의 실제 HTTPS origin으로
열립니다. Windows/모바일 사용자는 다운로드·확장·인증서·localhost 리스너가 필요 없습니다.

`AUTH_BIND_ADDRESS=127.0.0.1`이 기본이므로 bot의 `AUTH_PORT`는 proxy/tunnel upstream으로만
수신합니다. LAN 직접 수신이 정말 필요하면 방화벽을 먼저 제한한 후에만 이 값을 LAN 주소나
`0.0.0.0`으로 명시적으로 바꾸세요. nginx 예제는 WebSocket Upgrade와 `Host $http_host`를
전달해 non-default port도 보존합니다. 애플리케이션은 `AUTH_BASE_URL`의 정적 host/origin을
직접 검증하며 `X-Forwarded-*` 헤더로 인증 origin을 선택하지 않습니다.

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

### Raspberry Pi와 서버 인증

기본 `systemd` 배포는 비로그인 `valorant` 사용자와 `disabled` 모드로 실행되므로 Riot
Mobile QR을 사용합니다. headless Pi에서 `--remote-captcha`를 명시하면 별도 Xvfb와
Chromium 중계가 활성화되어 비밀번호 CAPTCHA도 사용할 수 있습니다. Xvfb가 시작되지
않더라도 soft `Wants=` 의존성 덕분에 봇과 QR은 계속 실행되고, 원격 비밀번호 방식만
실패 닫힘 처리됩니다. HTTPS/Xvfb/Chromium을 준비하지 않는 VPS·Docker는 QR을 사용하세요.

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

QR과 `disabled` 모드는 인바운드 포트·Tunnel이 전혀 필요 없습니다. `remote`에서만 안정적인
공개 HTTPS `AUTH_BASE_URL`과 WebSocket 지원 Tunnel/프록시가 필요합니다.

Automated tests cover the local flow's state transitions and cleanup; they do
not demonstrate a successful login against a live Riot account.

## Files

- `install.sh` / `uninstall.sh` — systemd on Linux
- `valorant-bot.service` — unit file
- `remote-captcha.conf` — 원격 모드 전용 환경 파일 (설치 시 base env와 분리)
- `valorant-captcha-display.service` — private Xvfb display unit
- `valorant-captcha-display.tmpfiles` — private outer auth dir와 root-sticky nested socket dir
- `prepare-captcha-display-auth` — 부팅/서비스 시작마다 X11 cookie를 만드는 helper
- `validate-remote-captcha-origin.py` — 설치 변경 전 원격 HTTPS origin 검사기
- `nginx.example.conf` — optional reverse proxy
- `env.*.example` — environment templates
- `../scripts/pi-tunnel.sh` — Cloudflare quick tunnel (원격 CAPTCHA 테스트 또는 `/invite`)
- `../scripts/run-local-remote.sh` — 로컬 LTE 테스트: quick tunnel origin을 `.env`에 쓴 뒤 봇 시작
- `extract-trycloudflare-origin.py` — cloudflared 로그에서 quick tunnel origin 추출
- `write-remote-captcha-env.py` — `.env`에 터널 origin·loopback bind·remote 모드만 갱신
- `pi-cloudflare-tunnel.md` — Pi Xvfb/Chromium 및 HTTPS Tunnel 운영 절차

See the root [README](../README.md).
