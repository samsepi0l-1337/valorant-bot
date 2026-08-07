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
