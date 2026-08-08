# Raspberry Pi 원격 CAPTCHA와 Cloudflare Tunnel

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
   `https://valorant-bot.example.com`.
   `http://`, IP 주소만의 URL, query, fragment, userinfo는 사용할 수 없습니다.
2. Tunnel/프록시는 `AUTH_PORT`의 HTTP와 WebSocket Upgrade를 그대로 전달해야 합니다.
3. Pi 서비스 사용자 `valorant`가 실행할 Chromium과 Xvfb가 있어야 합니다.
4. 방화벽은 Cloudflare Tunnel을 위한 **아웃바운드** 연결만 허용하면 됩니다. Pi에 80/443
   또는 Xvfb 포트를 열지 마세요.

Raspberry Pi OS/Debian에서 의존성이 없으면 다음을 한 번 실행합니다.

```bash
sudo apt update
sudo apt install xvfb chromium
```

배포 스크립트의 원격 옵션은 의존성을 자동 설치하지 않습니다. 패키지를 설치한 뒤 다시
실행하세요.

## 설치와 활성화

이미 Pi에 설치된 봇을 원격 모드로 바꾸거나, 처음 설치할 때 다음을 실행합니다.

```bash
sudo AUTH_BASE_URL=https://valorant-bot.example.com \
  ./scripts/setup-pi.sh --remote-captcha
```

설치 후 `/etc/valorant-bot/env`에는 다음 값이 있어야 합니다.

```dotenv
AUTH_BASE_URL=https://valorant-bot.example.com
AUTH_PORT=8787
CAPTCHA_BROWSER_MODE=remote
CAPTCHA_DISPLAY=:99
```

Xvfb를 먼저 시작하고 봇을 재시작합니다.

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now valorant-captcha-display.service
sudo systemctl restart valorant-bot.service
sudo systemctl status valorant-captcha-display.service valorant-bot.service --no-pager
```

로그 확인:

```bash
sudo journalctl -u valorant-captcha-display.service -u valorant-bot.service -n 100 --no-pager
```

`valorant-captcha-display.service`는 `valorant` 사용자 소유의 `:99` 화면을 만들고, TCP listen을
비활성화합니다. Chromium 및 Xvfb가 없거나 HTTPS 설정이 유효하지 않으면 원격 비밀번호
로그인은 실패 닫힘으로 처리되고 QR은 계속 사용할 수 있습니다.

## Cloudflare Tunnel

지속 운영에는 이름 있는 Tunnel과 소유한 도메인을 사용하고, public hostname의 서비스는
`http://127.0.0.1:8787`로 지정합니다. Cloudflare 대시보드/`cloudflared` 설정에서 WebSocket
지원이 꺼지지 않았는지 확인하세요. `AUTH_BASE_URL`의 host는 public hostname과 정확히
일치해야 합니다.

개발·점검용 quick tunnel은 다음처럼 실행할 수 있습니다.

```bash
./scripts/pi-tunnel.sh 8787
```

출력된 `https://…trycloudflare.com` 주소를 `AUTH_BASE_URL`로 쓰고 봇을 재시작합니다.
quick tunnel 주소는 프로세스를 다시 시작할 때 바뀌므로 지속적인 Discord 링크 origin으로는
사용하지 마세요. 이 주소가 바뀌면 새 주소로 환경 변수를 수정하고 봇을 재시작해야 합니다.

## 운영 점검

다음을 확인한 뒤 Discord에서 `/auth` → 아이디/비밀번호를 실행합니다.

```bash
sudo systemctl is-active valorant-captcha-display.service
sudo systemctl is-active valorant-bot.service
sudo grep -E '^(AUTH_BASE_URL|CAPTCHA_BROWSER_MODE|CAPTCHA_DISPLAY)=' /etc/valorant-bot/env
```

Discord의 같은 사용자가 받은 링크를 외부 브라우저에서 열어 CAPTCHA 프레임과 클릭/휠이
보이는지 확인합니다. Windows에는 봇 호스트의 Chromium을 설치하거나 열 필요가 없습니다.
실제 Riot 로그인이나 MFA 성공 여부는 계정·공개 HTTPS origin이 준비된 운영자가 별도로
검증해야 하며, 이 문서는 그러한 라이브 테스트가 수행되었다고 주장하지 않습니다.

## 즉시 롤백

HTTPS Tunnel 또는 Xvfb를 중단해야 하면 먼저 원격 비밀번호 로그인을 QR 전용으로 바꾸고
봇을 재시작한 뒤 디스플레이 유닛을 중지합니다.

```bash
sudo sed -i 's/^CAPTCHA_BROWSER_MODE=.*/CAPTCHA_BROWSER_MODE=disabled/' /etc/valorant-bot/env
sudo systemctl restart valorant-bot.service
sudo systemctl disable --now valorant-captcha-display.service
sudo systemctl status valorant-bot.service --no-pager
```

`disabled`에서는 Riot Mobile QR은 계속 동작하고 비밀번호 CAPTCHA만 명확한 QR 안내와 함께
거절됩니다. 데스크톱 호스트의 기존 GUI 흐름으로 되돌리려면 `CAPTCHA_BROWSER_MODE=local`로
바꾸고, 해당 봇 프로세스가 실제 GUI 세션과 Chromium을 사용할 수 있는지 확인하세요.
