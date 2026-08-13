# Raspberry Pi 원격 CAPTCHA와 Cloudflare Tunnel

같은 LAN의 다른 PC에서만 캡차를 완료하면 Cloudflare Tunnel은 필수가 아닙니다.
그 경로는 [`lan-remote-captcha.md`](lan-remote-captcha.md)를 따르세요. 아래는
인터넷에서 뷰어를 열 때 쓰는 HTTPS 터널 절차입니다.

이 문서는 headless Raspberry Pi에서 `remote` CAPTCHA 모드를 운영하는 절차입니다.
사용자는 Discord에서 받은 짧은 수명의 일회용 링크만 열면 됩니다. Windows 프로그램,
브라우저 확장, VNC/RDP, 인증서, 사용자 측 localhost 서버는 설치하지 않습니다.

## 동작 경계

Pi의 Xvfb 위에서 일반 GUI Chromium이 Riot의 실제 HTTPS 주소를 엽니다. Cloudflare
Tunnel(또는 동등한 HTTPS 역방향 프록시)은 이 프로젝트의 `AUTH_PORT`에만 연결하며,
뷰어 HTML·WebSocket·입력 이벤트를 전달합니다. Riot 문서는 터널 hostname으로 프록시,
프레임 또는 재작성되지 않습니다.

Riot 아이디/비밀번호, 쿠키, MFA 코드, Chromium 프로필과 DevTools 연결은 Pi에만 남습니다.
뷰어로 나가는 것은 크기 제한 JPEG 프레임뿐이고, Pi로 돌아오는 것은 검증된 기본 포인터
이벤트와 세로 휠 이벤트뿐입니다. 키보드, JavaScript, URL 탐색, 클립보드, 파일 전송,
추가 탭, DevTools TCP 포트, VNC는 중계하지 않습니다. Xvfb도 TCP로 수신하지 않습니다.

MFA는 CAPTCHA 뷰어가 아니라 기존 Discord MFA 버튼과 모달로 계속 입력합니다. 링크를
교환한 뒤 CAPTCHA가 완료·취소·만료되거나 연결 유예 시간이 지나면 뷰어와 브라우저가
종료됩니다.

## 사전 조건

1. 공개 DNS 이름과 TLS가 준비된 절대 HTTPS 주소가 필요합니다. 예:
   `https://valorant-bot.example.com`. hostname을 권장합니다. HTTPS IP 주소는 그 IP 주소에
   유효한 TLS 인증서가 있을 때만 사용할 수 있습니다. host는 비어 있으면 안 되고,
   `http://`, query, fragment, userinfo, 공백·제어문자, `/` 외의 path는 사용할 수 없습니다.
2. Tunnel/프록시는 `AUTH_PORT`의 HTTP와 WebSocket Upgrade를 그대로 전달해야 합니다.
3. Pi 서비스 사용자 `valorant`가 실행할 Chromium, Xvfb, `xauth`, `mcookie`와 설치 전
   origin 검사에 쓰는 Python 3가 있어야 합니다.
4. 방화벽은 Cloudflare Tunnel을 위한 **아웃바운드** 연결만 허용하면 됩니다. Pi에 80/443
   또는 Xvfb 포트를 열지 마세요.

`AUTH_BIND_ADDRESS=127.0.0.1`이 기본이므로 bot의 `AUTH_PORT`는 Pi 내부에서만 수신합니다.
의도적인 LAN 직접 수신이 필요한 경우에만 방화벽을 먼저 제한하고 `AUTH_BIND_ADDRESS`를 LAN
주소 또는 `0.0.0.0`으로 명시적으로 override하세요. remote viewer는 여전히 안정적인 HTTPS
proxy/Tunnel을 거쳐야 합니다.

Raspberry Pi OS/Debian에서 의존성이 없으면 다음을 한 번 실행합니다.

```bash
sudo apt-get update && sudo apt-get install -y xvfb chromium xauth util-linux python3
```

배포 스크립트의 원격 옵션은 의존성을 자동 설치하지 않습니다. 패키지를 설치한 뒤 다시
실행하세요.

## 설치와 활성화

### 신규 설치

아직 `/etc/valorant-bot/env`가 없는 신규 Pi는 처음 설치할 때 원하는 HTTPS origin을
넘깁니다.

```bash
sudo AUTH_BASE_URL=https://valorant-bot.example.com \
  ./scripts/setup-pi.sh --remote-captcha
```

### 기존 설치 마이그레이션

이미 설치된 Pi는 `setup-pi.sh`를 실행하기 **전에** base 환경 파일의
`AUTH_BASE_URL`을 선택한 공개 HTTPS origin으로 바꿉니다. 먼저 저장소에 포함된 검사기로
값을 검증한 뒤 `sudoedit`로 기존 줄을 정확히 한 줄만 수정하세요. 나머지 환경 값·파일
권한은 유지됩니다. 설치 프로그램도 실제로 사용할 환경 파일에 `AUTH_BASE_URL`이 정확히
한 번만 있는지 확인하고, 모든 설치·systemd 변경 전에 같은 형식 검사를 다시 수행합니다.

```bash
CAPTCHA_AUTH_BASE_URL='https://valorant-bot.example.com'
python3 ./deploy/validate-remote-captcha-origin.py "$CAPTCHA_AUTH_BASE_URL"
sudoedit /etc/valorant-bot/env
sudo AUTH_BASE_URL="$CAPTCHA_AUTH_BASE_URL" ./scripts/setup-pi.sh --remote-captcha
unset CAPTCHA_AUTH_BASE_URL
```

마이그레이션 명령 자체는 서비스를 재시작하지 않습니다. 마지막 설치 명령이 원격 display
유닛과 bot 서비스의 설치·시작을 처리하므로, 그 전에 별도로 bot을 재시작하지 마세요.

`--remote-captcha`는 원격 설정을 두 층으로 설치·관리합니다.

1. 저장소의 `deploy/remote-captcha.conf`는 **원격 환경 파일**이며
   `/etc/valorant-bot/remote-captcha.conf`로 설치됩니다. 이 파일만
   `CAPTCHA_BROWSER_MODE=remote`와 `CAPTCHA_DISPLAY=:99`를 소유합니다.
2. 설치 프로그램이 별도로 생성하는
   `/etc/systemd/system/valorant-bot.service.d/remote-captcha.conf`는
   systemd **drop-in**입니다. 이것이 원격 환경 파일을 읽고,
   `valorant-captcha-display.service`를 `Wants=`/`After=`로 연결하며,
   root-sticky nested X11 Unix socket 디렉터리만 두 PrivateTmp namespace에 bind합니다.
   bot 쪽 bind는 source가 없으면 무시하는 systemd `-` prefix를 사용하므로 display 준비
   실패가 QR-capable bot의 시작 실패로 전파되지 않습니다. Xauthority는 별도의 outer
   `/run` 경로에 유지됩니다.
   `Requires=`를 사용하지 않으므로 Xvfb가 실패해도 bot과 Riot Mobile QR은 계속 동작하고,
   원격 비밀번호 CAPTCHA만 지역화된 오류로 실패 닫힘 처리됩니다.

따라서 `--remote-captcha`는 기본 `/etc/valorant-bot/env`에 원격 모드를 쓰지 않습니다.
그 파일은 공용 봇 설정만, 원격 환경 파일은 원격 display 설정만 가집니다. 수동으로
drop-in을 복사하거나 임의의 display unit 이름을 만들지 말고, 설정 변경 뒤에는
`systemctl daemon-reload`와 봇 재시작을 실행하세요.

설치 후 `/etc/valorant-bot/env`에는 다음 값이 있어야 합니다.

```dotenv
AUTH_BASE_URL=https://valorant-bot.example.com
AUTH_PORT=8787
AUTH_BIND_ADDRESS=127.0.0.1
CAPTCHA_BROWSER_MODE=disabled
```

원격 값은 설치된 `/etc/valorant-bot/remote-captcha.conf`에서 확인합니다.

```dotenv
CAPTCHA_BROWSER_MODE=remote
CAPTCHA_DISPLAY=:99
XAUTHORITY=/run/valorant-captcha-display/Xauthority
```

Xvfb를 먼저 시작하고 봇을 재시작합니다.

```bash
sudo systemctl daemon-reload
sudo systemctl enable valorant-captcha-display.service valorant-bot.service
sudo systemctl start valorant-captcha-display.service || \
  echo 'Xvfb 시작 실패: 원격 비밀번호 CAPTCHA는 사용할 수 없지만 QR은 계속 사용 가능'
sudo systemctl restart valorant-bot.service
sudo systemctl status valorant-captcha-display.service valorant-bot.service --no-pager
```

로그 확인:

```bash
sudo journalctl -u valorant-captcha-display.service -u valorant-bot.service -n 100 --no-pager
```

`valorant-captcha-display.service`는 outer `/run/valorant-captcha-display`를
`valorant:valorant` mode `0700`으로 유지합니다. Xtrans가 요구하는 socket 디렉터리는 그
아래 `X11-unix`이며 `root:root` mode `01777`입니다. 두 서비스는 이 nested 디렉터리만
`/tmp/.X11-unix`로 bind하고 TCP listen을 비활성화합니다. 서비스가 시작될 때마다
`mcookie`로 새 MIT-MAGIC-COOKIE-1을 만들며, Xvfb의 정확한 `-auth` 파일과 Chromium의
`XAUTHORITY`는 outer 디렉터리의 같은 mode `0600` 파일을 가리킵니다. cookie 값은 출력하거나
로그에 남기지 않습니다. Chromium·Xvfb·X11 인증 또는 HTTPS 설정이 유효하지 않으면 원격
비밀번호 로그인은 실패 닫힘으로 처리되고 QR은 계속 사용할 수 있습니다.

## Cloudflare Tunnel

지속 운영에는 이름 있는 Tunnel과 소유한 도메인을 사용하고, public hostname의 서비스는
`http://127.0.0.1:8787`로 지정합니다. Cloudflare 대시보드/`cloudflared` 설정에서 WebSocket
지원이 꺼지지 않았는지 확인하세요. `AUTH_BASE_URL`의 host는 public hostname과 정확히
일치해야 합니다. 애플리케이션은 이 정적 `AUTH_BASE_URL`의 host/origin을 직접 검증하며,
`X-Forwarded-*` 헤더를 신뢰해 인증 origin을 선택하지 않습니다.

개발·점검용 quick tunnel은 다음처럼 실행할 수 있습니다. 설치된 remote deployment asset을
감지해 remote 모드임을 표시하지만, quick tunnel은 **테스트 전용**입니다.

```bash
./scripts/pi-tunnel.sh 8787
```

출력된 `https://…trycloudflare.com` 주소를 `AUTH_BASE_URL`로 쓰고 봇을 재시작합니다.
quick tunnel 주소는 프로세스를 다시 시작할 때 바뀌므로 지속적인 Discord 링크 origin으로는
사용하지 마세요. 이 주소가 바뀌면 새 주소로 환경 변수를 수정하고 봇을 재시작해야 합니다.
QR 또는 `disabled` 모드는 이 tunnel이나 인바운드 포트 없이 동작합니다.

로컬 노트북에서 집 밖 LTE로 캡차 뷰어를 시험할 때는 봇보다 먼저 터널 origin이
있어야 합니다. `./scripts/run-local-remote.sh`가 cloudflared quick tunnel을 띄워
URL을 파싱한 다음, 그 origin을 `.env`의 `AUTH_BASE_URL`에 쓰고 loopback bind와
`CAPTCHA_BROWSER_MODE=remote`를 맞춘 뒤 봇을 시작합니다. Discord 비밀은
유지합니다. quick tunnel 주소는 켤 때마다 바뀌므로 스크립트가 `.env`를 다시
고칩니다.

## 운영 점검

다음을 확인한 뒤 Discord에서 `/auth` → 아이디/비밀번호를 실행합니다.

```bash
sudo systemctl is-active valorant-captcha-display.service
sudo systemctl is-active valorant-bot.service
sudo grep -E '^(AUTH_BASE_URL|AUTH_PORT)=' /etc/valorant-bot/env
sudo grep -E '^(CAPTCHA_BROWSER_MODE|CAPTCHA_DISPLAY|XAUTHORITY)=' /etc/valorant-bot/remote-captcha.conf
```

Discord의 같은 사용자가 받은 링크를 외부 브라우저에서 열어 CAPTCHA 프레임과 클릭/휠이
보이는지 확인합니다. Windows에는 봇 호스트의 Chromium을 설치하거나 열 필요가 없습니다.
실제 Riot 로그인이나 MFA 성공 여부는 계정·공개 HTTPS origin이 준비된 운영자가 별도로
검증해야 하며, 이 문서는 그러한 라이브 테스트가 수행되었다고 주장하지 않습니다.

## 즉시 롤백

HTTPS Tunnel 또는 Xvfb를 중단해야 하면 원격 drop-in과 원격 환경 파일을 제거한 뒤,
기본 환경 파일에서 QR 전용 모드를 명시합니다. `valorant-bot.service`를 먼저 멈춰
실행 중인 Chromium을 정리하고, drop-in 제거 후 daemon reload를 해야 display 의존성이
남지 않습니다.

```bash
sudo systemctl stop valorant-bot.service
sudo systemctl disable --now valorant-captcha-display.service
sudo rm -f /etc/systemd/system/valorant-bot.service.d/remote-captcha.conf
sudo rm -f /etc/valorant-bot/remote-captcha.conf
sudo rm -f /etc/systemd/system/valorant-captcha-display.service
sudo rm -f /etc/tmpfiles.d/valorant-captcha-display.conf
sudo rm -f /usr/local/libexec/valorant-bot/prepare-captcha-display-auth
sudo rmdir /usr/local/libexec/valorant-bot 2>/dev/null || true
sudo rm -f /run/valorant-captcha-display/Xauthority
sudo rmdir /run/valorant-captcha-display/X11-unix 2>/dev/null || true
sudo rmdir /run/valorant-captcha-display 2>/dev/null || true
sudo sed -i '/^CAPTCHA_BROWSER_MODE=/d; /^CAPTCHA_DISPLAY=/d' /etc/valorant-bot/env
printf 'CAPTCHA_BROWSER_MODE=disabled\n' | sudo tee -a /etc/valorant-bot/env >/dev/null
sudo systemctl daemon-reload
sudo systemctl start valorant-bot.service
sudo systemctl status valorant-bot.service --no-pager
```

`disabled`에서는 Riot Mobile QR은 계속 동작하고 비밀번호 CAPTCHA만 명확한 QR 안내와 함께
거절됩니다. 데스크톱 호스트의 기존 GUI 흐름으로 되돌리려면 `CAPTCHA_BROWSER_MODE=local`로
바꾸고, 해당 봇 프로세스가 실제 GUI 세션과 Chromium을 사용할 수 있는지 확인하세요.
