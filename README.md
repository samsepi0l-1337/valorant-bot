# Valorant Discord Bot

Discord에서 Valorant 일일 상점 조회, Riot 계정 연동, 위시리스트, 길드 알림을
제공하는 봇입니다. Go로 작성되었고 macOS · Windows · Linux · Raspberry Pi ·
Docker에서 실행할 수 있습니다.

`/auth`는 **Riot Mobile QR 스캔** 또는 **아이디/비밀번호 로그인**을 제공합니다.
별도 프로그램 설치, URL 붙여넣기, 사용자 측 localhost/포트 80은 필요 없습니다.
비밀번호 로그인은 봇이 실행 중인 데스크톱의 Chrome에서 Riot 캡차를 열고, Riot이
요구할 때만 Discord 모달로 MFA 코드를 받습니다. 디스플레이가 없는 Raspberry Pi는
QR 방식을 사용하세요.

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
| `AUTH_BASE_URL`    | 예     | `/invite`·선택적 보조 페이지 주소                     |
| `AUTH_PORT`        | 아니오 | HTTP 포트 (기본 `8787`)                              |
| `DATABASE_PATH`    | 아니오 | SQLite 경로                                          |
| `STORE_RESET_CRON` | 아니오 | 레거시 크론 문자열 (일일 시각은 `/channel time`)     |

템플릿: `deploy/env.local.example`, `deploy/env.pi.example`,
`deploy/env.server.example`, `.env.example`

`/auth`(QR·아이디 로그인)는 `AUTH_BASE_URL`과 무관합니다. 이 값은 `/invite` 등용입니다.

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
- **아이디/비밀번호**: Discord 모달에 계정 정보를 입력하고, 봇 호스트에서 열린
  Chrome의 「로봇이 아닙니다」를 완료합니다. Riot이 MFA를 요구하면 Discord 모달에
  이메일 또는 인증 앱 코드를 입력합니다.

QR은 약 3분 후 만료됩니다. 여러 Riot 계정은 `/auth`를 계정마다 반복하면 됩니다.
QR 방식은 봇 서버가 Riot 세션 생성·승인 폴링·토큰 교환을 모두 수행하고 휴대폰은
Riot Mobile에서 승인만 합니다. 토큰 응답 안의 `http://localhost/redirect`는 Riot
Client에 등록된 결과 URI를 파싱하기 위한 값일 뿐 브라우저가 열거나 접속하지 않으므로,
원격 서버도 Riot/Discord로 나가는 연결만 있으면 됩니다.

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
