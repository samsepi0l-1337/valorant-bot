# LAN HTTP 원격 CAPTCHA (터널 없이)

같은 Wi-Fi/LAN의 **다른 컴퓨터**에서 비밀번호 로그인 캡차를 완료할 때 씁니다.
Cloudflare Tunnel은 필요 없습니다. 인터넷에서 뷰어를 열려면 기존 HTTPS 터널 경로
([`pi-cloudflare-tunnel.md`](pi-cloudflare-tunnel.md))를 사용하세요.

봇 호스트 Chrome은 항상 실제 `https://authenticate.riotgames.com`을 엽니다.
뷰어는 JPEG 프레임과 포인터/휠만 중계합니다. Riot 아이디/비밀번호·세션 쿠키는
봇 호스트에만 남습니다. MFA는 캡차 뷰어가 아니라 **Discord 모달**입니다.

공개 HTTP 호스트(`http://relay.example.com` 등)는 거부됩니다.

## 노트북 (macOS / Linux GUI)

1. `deploy/env.local.example`을 `.env`로 복사한 뒤 LAN 값을 켭니다.

```bash
AUTH_BASE_URL=http://192.168.0.10:8787
AUTH_BIND_ADDRESS=0.0.0.0
AUTH_PORT=8787
CAPTCHA_BROWSER_MODE=remote
```

`<lan-ip>`는 봇이 실행 중인 노트북의 LAN 주소입니다. `AUTH_BIND_ADDRESS`를
`127.0.0.1`로 두면 다른 PC가 뷰어에 연결하지 못합니다.

2. 방화벽은 **LAN에서 `AUTH_PORT`만** 엽니다. Xvfb 포트는 열지 않습니다.
   macOS 원격 모드도 `CAPTCHA_DISPLAY` 설정은 필요하지만, 로컬 GUI Chrome에는
   `DISPLAY=:99`를 주입하지 않습니다.

3. `go run ./cmd/bot` 또는 `make run`으로 봇을 실행합니다.

4. 같은 네트워크의 다른 PC에서 Discord `/auth` → 아이디/비밀번호 → 링크를 엽니다.
   캡차 후 Riot이 요구하면 Discord가 2FA 코드를 묻습니다.

## Raspberry Pi

1. Chromium과 (headless면) Xvfb를 설치합니다. 인터넷 Tunnel 없이 LAN만 쓸 때도
   remote drop-in의 Xvfb/Chromium은 필요합니다.

2. `/etc/valorant-bot/env` 예시:

```bash
AUTH_BASE_URL=http://192.168.0.10:8787
AUTH_BIND_ADDRESS=0.0.0.0
AUTH_PORT=8787
```

`CAPTCHA_BROWSER_MODE=remote`는 `--remote-captcha` drop-in이 담당합니다.
`--remote-captcha` + 공개 HTTPS Tunnel은 인터넷 경로이고, 위 HTTP 값은 같은
LAN 전용입니다.

3. 방화벽: LAN → Pi `AUTH_PORT`만 허용. Xvfb는 TCP로 열지 않습니다.

4. 다른 PC의 브라우저에서 Discord가 준 `http://<pi-lan-ip>:8787/captcha/remote#…`
   링크를 엽니다. 휴대폰이 LTE면 같은 LAN이 아니므로 실패합니다.

## HTTP 주의

LAN HTTP는 뷰어 **세션 쿠키**가 평문으로 갑니다. Riot 자격 증명은 봇 Chrome에
남고 뷰어로 전달되지 않습니다. 신뢰할 수 있는 홈/사무실 LAN에서만 쓰세요.
이 문서는 배포 설정만 다루며, 실제 Riot 로그인 성공을 검증한 기록이 아닙니다.
