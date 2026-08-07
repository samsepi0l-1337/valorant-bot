package i18n

import "testing"

func TestCaptchaCopyDescribesButtonStartedChrome(t *testing.T) {
	tests := []struct {
		lang     Lang
		prompt   string
		button   string
		launched string
	}{
		{
			lang:     KO,
			prompt:   "아래 버튼을 누르면 봇이 실행 중인 이 Mac/PC에서 Chrome 캡차 창을 엽니다.\n그 창에서 **「로봇이 아닙니다」**만 체크하세요.",
			button:   "캡차 창 열기",
			launched: "봇 PC에서 Chrome 캡차 창을 열었습니다.",
		},
		{
			lang:     EN,
			prompt:   "Tap the button below to open a Chrome captcha window on the bot Mac/PC.\nCheck **I'm not a robot** in that window.",
			button:   "Open captcha window",
			launched: "Opened the Chrome captcha window on the bot machine.",
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.lang), func(t *testing.T) {
			if got := T(tt.lang, "auth.captcha.prompt"); got != tt.prompt {
				t.Fatalf("prompt = %q", got)
			}
			if got := T(tt.lang, "auth.captcha.button"); got != tt.button {
				t.Fatalf("button = %q", got)
			}
			if got := T(tt.lang, "auth.captcha.launched"); got != tt.launched {
				t.Fatalf("launched = %q", got)
			}
		})
	}
}

func TestMFAOwnerAndTerminalCopy(t *testing.T) {
	tests := []struct {
		lang     Lang
		denied   string
		expired  string
		terminal string
	}{
		{
			lang:     KO,
			denied:   "이 인증 코드는 로그인 요청을 시작한 본인만 입력할 수 있습니다.",
			expired:  "2차 인증 세션이 만료되었습니다. `/auth` 를 다시 실행해 주세요.",
			terminal: "2차 인증에 실패했습니다: %v\n`/auth` 를 다시 실행해 주세요.",
		},
		{
			lang:     EN,
			denied:   "Only the user who started this login can enter its verification code.",
			expired:  "The verification session expired. Run `/auth` again.",
			terminal: "2FA failed: %v\nRun `/auth` again.",
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.lang), func(t *testing.T) {
			if got := T(tt.lang, "auth.mfa.denied"); got != tt.denied {
				t.Fatalf("denied = %q", got)
			}
			if got := T(tt.lang, "auth.mfa.expired"); got != tt.expired {
				t.Fatalf("expired = %q", got)
			}
			if got := T(tt.lang, "auth.mfa.failed"); got != tt.terminal {
				t.Fatalf("terminal = %q", got)
			}
		})
	}
}
