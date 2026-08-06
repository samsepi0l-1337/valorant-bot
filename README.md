# Valorant Discord Bot

Discord에서 Valorant 일일 상점 조회, Riot 계정 연동, 위시리스트, 길드 알림을
제공하는 봇입니다. Go로 작성되었고 macOS · Windows · Linux · Raspberry Pi ·
Docker에서 실행할 수 있습니다.

## 목차

- [Discord 앱 설정](#discord-앱-설정)
- [환경 변수](#환경-변수)
- [OS별 실행 방법](#os별-실행-방법)
  - [macOS](#macos)
  - [Windows](#windows)
  - [Linux (일반 PC / VPS)](#linux-일반-pc--vps)
  - [Raspberry Pi](#raspberry-pi)
  - [Docker](#docker)
- [Riot 계정 연동](#riot-계정-연동-auth)
- [슬래시 명령](#슬래시-명령)
- [빌드 치트시트](#빌드-치트시트)
- [스케줄러](#스케줄러)

---

## Discord 앱 설정

1. [Discord Developer Portal](https://discord.com/developers/applications)에서 앱 생성
2. **Bot** 탭에서 토큰 발급 → `DISCORD_TOKEN`
3. **OAuth2 → General**에서 Client ID 확인 → `DISCORD_APP_ID`
4. Privileged Gateway Intents는 기본값으로 충분합니다 (`Guilds`, `Guild Messages`).
   Message Content Intent는 필요 없습니다.

### 봇 초대

```
https://discord.com/oauth2/authorize?client_id=YOUR_APP_ID
```

서버에 초대되면 해당 길드에 슬래시 명령이 바로 등록됩니다.
실행 중이면 `http://<AUTH_BASE_URL>/invite` 로도 초대 링크를 열 수 있습니다.

---

## 환경 변수

템플릿:

| 파일 | 용도 |
|------|------|
| `.env.example` | 공통 |
| `deploy/env.local.example` | 로컬 PC |
| `deploy/env.pi.example` | Raspberry Pi |
| `deploy/env.server.example` | VPS / 서버 |

| 변수 | 필수 | 설명 |
|------|------|------|
| `DISCORD_TOKEN` | 예 | 봇 토큰 |
| `DISCORD_APP_ID` | 예 | 애플리케이션 ID |
| `BOT_SECRET` | 예 | 세션 암호화 키 (**32자 이상**) |
| `AUTH_BASE_URL` | 예 | 브라우저가 여는 인증 URL |
| `AUTH_PORT` | 아니오 | 인증 HTTP 포트 (기본 `8787`) |
| `DATABASE_PATH` | 아니오 | SQLite 경로 (기본 `./data/bot.db`) |
| `STORE_RESET_CRON` | 아니오 | 일일 상점 크론 (기본 `0 0 * * *` UTC) |

**`AUTH_BASE_URL` 규칙**

| 사용 방식 | 예시 |
|-----------|------|
| 봇과 같은 PC에서만 로그인 | `http://127.0.0.1:8787` |
| 같은 Wi‑Fi의 다른 PC/폰에서 로그인 | `http://192.168.0.37:8787` (봇 기기의 LAN IP) |
| 공개 서버 + 도메인 | `https://bot.example.com` |

`127.0.0.1`로 두면 **다른 기기에서는 로그인할 수 없습니다.**

---

## OS별 실행 방법

공통 준비:

```bash
git clone https://github.com/samsepi0l-1337/valorant-bot.git
cd valorant-bot
cp .env.example .env
# .env 에 DISCORD_TOKEN, DISCORD_APP_ID, BOT_SECRET, AUTH_BASE_URL 입력
```

Go 1.22+ 가 있으면 소스에서 바로 실행할 수 있고, 없으면 미리 빌드한 바이너리만
복사해도 됩니다 (`CGO_ENABLED=0` 정적 빌드).

### macOS

**요구:** [Go](https://go.dev/dl/) 설치 (또는 미리 빌드한 `bin/valorant-bot`)

```bash
cp deploy/env.local.example .env
nano .env
# AUTH_BASE_URL=http://127.0.0.1:8787          # 이 Mac에서만 로그인
# AUTH_BASE_URL=http://192.168.x.x:8787        # 다른 기기에서도 로그인

make run
# 또는
make build && ./bin/valorant-bot
```

`/auth` **자동** 연동(포트 80)이 필요하면:

```bash
make build
sudo ./bin/valorant-bot
```

Apple Silicon / Intel 모두 `make build`로 현재 Mac용 바이너리가 나옵니다.
배포용으로 둘 다 만들려면 `make build-darwin` 을 사용하세요.

방화벽에서 `AUTH_PORT`(기본 8787) 수신을 허용해야 다른 기기가 붙을 수 있습니다.

### Windows

**방법 A — Go로 실행 (권장, 개발용)**

1. [Go](https://go.dev/dl/) 설치 후 PowerShell:

```powershell
git clone https://github.com/samsepi0l-1337/valorant-bot.git
cd valorant-bot
copy .env.example .env
notepad .env
go run .\cmd\bot
```

**방법 B — 리눅스용처럼 크로스 빌드하지 않고 Windows 바이너리 직접 빌드**

```powershell
go build -o valorant-bot.exe .\cmd\bot
.\valorant-bot.exe
```

**방법 C — WSL2 (Ubuntu)**

WSL 안에서 [Linux](#linux-일반-pc--vps) 절과 동일하게 `make run` / systemd 없이
포그라운드 실행하면 됩니다. `AUTH_BASE_URL`은 Windows 호스트 LAN IP 또는
WSL IP를 상황에 맞게 넣으세요.

> Windows에서 Riot `localhost/redirect`(포트 80) 자동 캡처는 관리자 권한이
> 필요할 수 있습니다. 다른 PC 연동은 **URL 붙여넣기**를 쓰는 편이 쉽습니다.

### Linux (일반 PC / VPS)

**개발 PC에서 바로 실행**

```bash
cp deploy/env.local.example .env   # 또는 env.server.example
nano .env
make run
# 또는
make build && ./bin/valorant-bot
```

**VPS에 systemd로 설치 (amd64)**

개발 머신에서:

```bash
make build-linux
scp dist/valorant-bot-linux-amd64 user@server:~/
scp -r deploy user@server:~/valorant-deploy
```

서버에서:

```bash
cp ~/valorant-deploy/env.server.example /tmp/valorant.env
nano /tmp/valorant.env
# AUTH_BASE_URL=https://bot.example.com  또는  http://서버공인IP:8787

sudo ~/valorant-deploy/install.sh \
  --binary ~/valorant-bot-linux-amd64 \
  --env /tmp/valorant.env

sudo journalctl -u valorant-bot -f
```

설치 위치:

| 경로 | 내용 |
|------|------|
| `/usr/local/bin/valorant-bot` | 바이너리 |
| `/etc/valorant-bot/env` | 환경 변수 |
| `/var/lib/valorant-bot/data/` | SQLite |
| `systemd: valorant-bot` | 서비스 |

제거: `sudo ./deploy/uninstall.sh` (데이터까지 삭제: `--purge`)

HTTPS는 `deploy/nginx.example.conf` 참고.

### Raspberry Pi

봇만 Pi에 두고, 로그인은 **폰/PC 브라우저 + URL 붙여넣기**를 쓰는 구성을 권장합니다.

**1) Mac/PC에서 크로스 빌드**

```bash
# 64비트 Pi (Pi 3/4/5 64-bit OS) — 권장
make build-pi
scp dist/valorant-bot-linux-arm64 pi@raspberrypi:~/

# 32비트 Pi
make build-pi32
scp dist/valorant-bot-linux-armv7 pi@raspberrypi:~/
```

또는 한 줄 배포:

```bash
./scripts/deploy-remote.sh pi@192.168.0.10 --pi
```

**2) Pi에서 환경 설정 · 설치**

```bash
# 저장소의 deploy/ 가 있다고 가정
cp deploy/env.pi.example /tmp/valorant.env
nano /tmp/valorant.env
# AUTH_BASE_URL=http://<Pi의-LAN-IP>:8787   ← 반드시 LAN IP

sudo ./deploy/install.sh \
  --binary ~/valorant-bot-linux-arm64 \
  --env /tmp/valorant.env

sudo systemctl status valorant-bot
sudo journalctl -u valorant-bot -f
```

Pi LAN IP 확인: `hostname -I`

> Pi에 모니터/브라우저가 없어도 됩니다. Discord `/auth` → 로그인 페이지가
> Pi IP로 열리면, Riot 로그인 후 주소창의 `localhost/redirect#...` 를
> 붙여넣으면 연동됩니다.

### Docker

macOS / Linux / Windows(Docker Desktop) 공통:

```bash
cp deploy/env.server.example .env.docker
# AUTH_BASE_URL 을 호스트에서 접근 가능한 주소로 설정
docker compose --env-file .env.docker up -d --build
docker compose logs -f
```

데이터는 Docker volume `valorant_bot_data`에 저장됩니다.

---

## Riot 계정 연동 (`/auth`)

1. Discord에서 `/auth` → **로그인** 버튼
2. 연동 페이지에서 **Riot으로 로그인**
3. **봇과 같은 기기:** `http://localhost/redirect` 자동 캡처  
   (포트 80 필요 → macOS/Linux에서는 보통 `sudo ./valorant-bot`)
4. **다른 기기:** 로그인 후 주소창의  
   `http://localhost/redirect#access_token=...` **전체**를 연동 페이지 폼에 붙여넣기
5. Discord DM으로 연동 완료 알림

---

## 슬래시 명령

| 명령 | 설명 |
|------|------|
| `/auth` | Riot 계정 연동 |
| `/accounts` | 연결된 계정 목록 |
| `/unlink` | 계정 연결 해제 |
| `/shop` | 오늘 상점 (언어 설정에 맞는 스킨 이름) |
| `/wishlist add\|remove\|list` | 위시리스트 (추가/제거 시 선택 메뉴) |
| `/channel set` | 일일 알림 채널 지정 |
| `/language` | 봇 응답·스킨 이름 언어 (`ko` / `en`) |

---

## 빌드 치트시트

```bash
make help
make build          # 현재 OS → bin/valorant-bot
make build-pi       # linux/arm64 (Raspberry Pi 64-bit)
make build-pi32     # linux/armv7 (Raspberry Pi 32-bit)
make build-linux    # linux/amd64 (VPS)
make build-darwin   # macOS arm64 + amd64
make build-all      # dist/ 전부
make test
```

---

## 스케줄러

일일 길드 알림·위시리스트 DM은 `STORE_RESET_CRON`(기본 `0 0 * * *` UTC ≈ 09:00 KST)
에 실행됩니다. `/channel set`으로 채널을 지정하세요.
