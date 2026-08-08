# Valorant Discord Bot

Discord에서 Valorant 일일 상점 조회, Riot 계정 연동, 위시리스트, 길드 알림을
제공하는 봇입니다. Go로 작성되었고 macOS · Windows · Linux · Raspberry Pi ·
Docker에서 실행할 수 있습니다.

`/auth`는 **Riot Mobile QR 스캔** 또는 **아이디/비밀번호 로그인**을 제공합니다.
별도 프로그램 설치, URL 붙여넣기, 사용자 측 localhost/포트 80은 필요 없습니다.
기본 `local` 모드의 비밀번호 로그인은 **봇이 실행 중인 호스트**의 GUI Chrome에서
Riot 공식 로그인 페이지를 엽니다. `remote` 모드는 headless Pi/서버의 GUI Chromium을
짧은 수명의 Discord 링크로 중계해 Windows·모바일 브라우저에서 CAPTCHA를 완료합니다.
Riot이 요구할 때만 Discord 모달로 MFA 코드를 받습니다. HTTPS/Xvfb/Chromium을 준비할
수 없는 headless 호스트는 `disabled`로 두고 QR 방식을 사용하세요.

## 빠른 시작 (원샷 스크립트)

Discord Developer Portal에서 앱을 만들고 **Bot 토큰**(`DISCORD_TOKEN`)과
**Application ID**(`DISCORD_APP_ID`)를 준비합니다.

### 로컬 (macOS / Linux / WSL)

```bash
git clone https://github.com/samsepi0l-1337/valorant-bot.git
cd valorant-bot
./scripts/setup-local.sh --run
```

토큰·App ID를 물어본 뒤 `.env` 작성, 빌드, 실행까지 합니다.

```bash
DISCORD_TOKEN=... DISCORD_APP_ID=... ./scripts/setup-local.sh --yes --run
```

### 클라우드 / VPS (linux/amd64)

노트북에서 원격 설치:

```bash
./scripts/setup-cloud.sh --host user@vps.example.com
```

서버에서 직접 (저장소 클론 후):

```bash
sudo ./scripts/setup-cloud.sh
# 또는 Docker
sudo ./scripts/setup-cloud.sh --docker
```

### Raspberry Pi

노트북에서 크로스 빌드 후 설치 (권장, 64-bit OS):

```bash
./scripts/setup-pi.sh --host pi@192.168.0.10
# 32-bit OS
./scripts/setup-pi.sh --host pi@192.168.0.10 --arch armv7
```

Pi에서 직접:

```bash
sudo ./scripts/setup-pi.sh
```

봇 초대:

```
https://discord.com/oauth2/authorize?client_id=YOUR_APP_ID
```

---

## 환경 변수

| 변수               | 필수   | 설명                                                 |
| ------------------ | ------ | ---------------------------------------------------- |
| `DISCORD_TOKEN`    | 예     | 봇 토큰                                              |
| `DISCORD_APP_ID`   | 예     | 애플리케이션 ID                                      |
| `BOT_SECRET`       | 예     | 세션 암호화 키 (**32자 이상**, 스크립트가 생성 가능) |
| `AUTH_BASE_URL`    | 예     | `/invite` 주소; 원격 모드에서는 정확한 공개 HTTPS 주소 |
| `AUTH_PORT`        | 아니오 | HTTP 포트 (기본 `8787`)                              |
| `CAPTCHA_BROWSER_MODE` | 아니오 | `local`(기본), `remote`, `disabled`              |
| `CAPTCHA_DISPLAY`  | 원격 시 예 | 원격 Chromium 디스플레이 (Pi 기본 `:99`)            |
| `DATABASE_PATH`    | 아니오 | SQLite 경로                                          |
| `STORE_RESET_CRON` | 아니오 | 레거시 크론 문자열 (일일 시각은 `/channel time`)     |

템플릿: `deploy/env.local.example`, `deploy/env.pi.example`,
`deploy/env.server.example`, `.env.example`

`CAPTCHA_BROWSER_MODE=local`은 기존처럼 봇 호스트의 GUI Chrome/Chromium 창을
사용합니다. `disabled`는 비밀번호 CAPTCHA 시작을 명확히 거절하고 Riot Mobile QR을
안내합니다. `remote`는 봇 호스트의 GUI Chromium 화면을 Discord 사용자의 브라우저로
중계합니다. 이 경우 `AUTH_BASE_URL`은 쿼리·fragment·사용자 정보가 없는 절대
`https://` 공개 주소여야 하며, 프록시/터널은 WebSocket을 통과시켜야 합니다.
사용자 PC에는 Windows용 다운로드, 확장 프로그램, localhost 리스너가 필요 없습니다.
Pi 원격 배포는 [`deploy/pi-cloudflare-tunnel.md`](deploy/pi-cloudflare-tunnel.md)를
따르세요.

---

## Discord 앱 설정

1. [Discord Developer Portal](https://discord.com/developers/applications)에서
   앱 생성
2. **Bot** → 토큰 → `DISCORD_TOKEN`
3. **OAuth2 → General** → Client ID → `DISCORD_APP_ID`
4. Privileged Gateway Intents는 기본값으로 충분합니다

서버에 초대되면 해당 길드에 슬래시 명령이 등록됩니다.

---

## 수동 실행 (스크립트 없이)

```bash
cp deploy/env.local.example .env   # 값 편집
make run                           # 또는 make build && ./bin/valorant-bot
```

Windows (PowerShell):

```powershell
copy .env.example .env
go run .\cmd\bot
```

Docker:

```bash
cp deploy/env.server.example .env.docker
docker compose --env-file .env.docker up -d --build
```

systemd 위치: `/usr/local/bin/valorant-bot`, `/etc/valorant-bot/env`,
`/var/lib/valorant-bot/data/`, 유닛 `valorant-bot`.  
제거: `sudo ./deploy/uninstall.sh` (`--purge` 시 데이터 포함).

---

## Riot 계정 연동 (`/auth`)

Discord에서 `/auth`를 실행한 뒤 원하는 방식을 선택합니다.

- **Riot Mobile QR**: QR을 스캔(또는 **Riot Mobile로 열기**)하고 앱에서 승인합니다.
- **아이디/비밀번호 (`local`)**: 다음 순서로 진행합니다.
  1. Discord 모달에 계정 정보를 입력합니다.
  2. 그 요청의 소유자가 Discord의 **캡차 창 열기/다시 열기** 버튼을 누릅니다.
  3. 봇 호스트에서 Riot 공식 GUI Chrome 창이 열리면 「로봇이 아닙니다」를
     완료합니다. Chrome은 Riot의 실제 DNS·TLS를 사용하고, 봇은 외부에 노출되지 않는
     전용 DevTools pipe로 이 한 번의 로그인 창만 제어합니다.
  4. 캡차 결과가 처리되면 봇이 Chrome 창을 닫습니다. Riot이 MFA를 요구한 경우에만
     Discord의 MFA 버튼을 누르고 모달에 이메일 또는 인증 앱 코드를 입력합니다.

`remote`에서는 Discord의 같은 사용자가 받은 일회용·짧은 수명의 **Open remote CAPTCHA**
링크를 엽니다. 링크의 bearer 값은 URL fragment에만 있고 첫 HTTP 요청에 전송되지 않으며,
교환 직후 브라우저 기록에서 지워집니다. Pi/서버의 GUI Chromium이 Riot 공식 HTTPS
페이지를 계속 열고, 사용자는 중계된 프레임을 보며 포인터·휠만 조작합니다. CAPTCHA가
끝나면 뷰어와 Chromium이 닫히며, Riot이 MFA를 요구할 때는 기존처럼 Discord MFA 버튼과
모달에서만 코드를 입력합니다. Discord 사용자는 Chrome, 인증 도우미, Windows 설치물,
localhost 주소 또는 포트를 설치·실행하지 않습니다.

각 비밀번호 로그인 흐름에는 활성 뷰어를 하나만 연결할 수 있습니다. 뷰어/교환 grant의
수명은 기존 비밀번호 로그인 TTL 또는 최대 10분 중 더 이른 시점까지이며, 활동으로 연장되지
않습니다. 정상 연결이 끊겨도 같은 브라우저 세션은 60초 안에 재연결할 수 있고, 그 시간이
지나면 흐름은 취소되어 자격 증명과 브라우저 상태가 정리됩니다.

원격 뷰어는 Riot 문서나 DevTools를 인터넷에 노출하지 않습니다. Riot 자격 증명, 쿠키,
MFA 코드, 브라우저 저장소 및 CDP 연결은 봇 호스트에만 남습니다. 중계 채널은 크기가
제한된 JPEG 프레임과 검증된 기본 포인터/휠 이벤트만 허용하며, 키보드 입력, JavaScript,
탐색, 클립보드, 파일 전송 및 추가 탭은 허용하지 않습니다. `local`에는 macOS 또는 Linux
봇 호스트의 GUI Chrome/Chromium이 필요합니다. HTTPS·Xvfb·Chromium을 준비할 수 없는
headless Pi/VPS/Docker는 `disabled`로 두고 Riot Mobile QR을 사용하세요. Arduino 같은
마이크로컨트롤러는 이 Go 애플리케이션을 실행하는 지원 대상이 아닙니다.

QR은 약 3분 후 만료됩니다. 여러 Riot 계정은 `/auth`를 계정마다 반복하면 됩니다.
QR 방식은 봇 서버가 Riot 세션 생성·승인 폴링·토큰 교환을 모두 수행하고 휴대폰은
Riot Mobile에서 승인만 합니다. 토큰 응답 안의 `http://localhost/redirect`는 Riot
Client에 등록된 결과 URI를 파싱하기 위한 값일 뿐, 사용자 또는 봇에 localhost 브라우저
접속을 요구하지 않습니다. QR에는 Riot/Discord로 나가는 연결만 있으면 됩니다.

이 흐름은 자동화 테스트로 상태 전이와 정리를 검증합니다. 실제 Riot 계정으로의 인증
성공은 운영자가 직접 로그인하지 않는 한 보장하거나 주장하지 않습니다.

Riot의 공식 제3자 인증인 [RSO](https://developer.riotgames.com/docs/valorant)는 승인된
Production 애플리케이션과 RSO Client가 필요합니다. 공개 VALORANT API에는 개인 일일
상점 조회가 없어 현재 상점 세션을 그대로 대체할 수 없으므로, 이 봇은 QR 승인을
서버 측에서 교환하는 방식을 사용합니다.

---

## 슬래시 명령

| 명령                          | 설명                            |
| ----------------------------- | ------------------------------- |
| `/auth`                       | QR 또는 아이디/비밀번호로 연동  |
| `/accounts`                   | 연결된 계정 목록                |
| `/unlink`                     | 계정 연결 해제                  |
| `/shop`                       | 오늘 상점                       |
| `/wishlist add\|remove\|list` | 위시리스트                      |
| `/channel set`                | 일일 알림 채널                  |
| `/channel time`               | 일일 알림 시각 (KST)            |
| `/language`                   | 봇·스킨 이름 언어 (`ko` / `en`) |

---

## 빌드

```bash
make help
make build          # 현재 OS → bin/valorant-bot
make build-pi       # linux/arm64
make build-pi32     # linux/armv7
make build-linux    # linux/amd64
make build-darwin   # macOS arm64 + amd64
make test
```

## 스케줄러

길드별 `/channel time`에서 고른 **KST 시각**(기본 09:00)에 일일 상점 알림과
위시리스트 DM을 보냅니다.
