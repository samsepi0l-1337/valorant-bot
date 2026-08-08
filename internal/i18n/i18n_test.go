package i18n

import (
	"strings"
	"testing"
)

func TestCaptchaCopyDescribesButtonStartedChrome(t *testing.T) {
	tests := []struct {
		lang     Lang
		prompt   string
		button   string
		launched string
	}{
		{
			lang:     KO,
			prompt:   "아래 버튼을 누르면 봇이 실행 중인 Mac/Linux PC에서 Riot 공식 Chrome 캡차 창을 엽니다.\n그 창에 표시되는 **「로봇이 아닙니다」** 캡차를 완료하세요.",
			button:   "캡차 창 열기",
			launched: "봇 PC에서 Chrome 캡차 창을 열었습니다.",
		},
		{
			lang:     EN,
			prompt:   "Tap below to open Riot's official Chrome captcha window on the bot macOS/Linux machine.\nComplete the **I'm not a robot** challenge shown there.",
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

func TestRemoteCaptchaRelayCopyIsLocalizedAndExplicit(t *testing.T) {
	tests := []struct {
		lang      Lang
		prompt    string
		open      string
		cancel    string
		cancelled string
		denied    string
	}{
		{
			lang:      KO,
			prompt:    "아래 보안 릴레이를 열어 봇 호스트에서 실행 중인 Chrome의 Riot 캡차를 완료하세요. 렌더링된 캡차 화면과 포인터 입력만 중계됩니다. 이 링크는 곧 만료됩니다.",
			open:      "원격 캡차 열기",
			cancel:    "로그인 취소",
			cancelled: "Riot 로그인 요청을 취소했습니다. 원격 캡차 창을 닫아도 됩니다.",
			denied:    "이 로그인 요청은 시작한 본인만 취소할 수 있습니다.",
		},
		{
			lang:      EN,
			prompt:    "Open the secure relay below to complete Riot's captcha in Chrome running on the bot host. Only rendered captcha frames and pointer input are relayed. This link expires shortly.",
			open:      "Open remote captcha",
			cancel:    "Cancel login",
			cancelled: "Canceled the Riot login. You can close the remote captcha window.",
			denied:    "Only the user who started this login can cancel it.",
		},
	}
	for _, test := range tests {
		t.Run(string(test.lang), func(t *testing.T) {
			for key, want := range map[string]string{
				"auth.captcha.remote.prompt": test.prompt,
				"auth.captcha.remote.open":   test.open,
				"auth.captcha.cancel":        test.cancel,
				"auth.captcha.cancelled":     test.cancelled,
				"auth.captcha.cancel.denied": test.denied,
			} {
				if got := T(test.lang, key); got != want {
					t.Errorf("%s=%q, want %q", key, got, want)
				}
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

func TestMFACopyConsistentlyDescribesSixToEightDigits(t *testing.T) {
	tests := []struct {
		lang i18nTestLang
		want string
	}{
		{lang: i18nTestLang{code: KO, marker: "6~8자리"}, want: "ko"},
		{lang: i18nTestLang{code: EN, marker: "6–8 digit"}, want: "en"},
	}
	keys := []string{
		"auth.mfa.code",
		"auth.mfa.code_email",
		"auth.mfa.code_app",
		"auth.mfa.prompt",
		"auth.mfa.prompt_email",
		"auth.mfa.prompt_email_generic",
		"auth.mfa.prompt_app",
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			for _, key := range keys {
				if got := T(tt.lang.code, key); !strings.Contains(got, tt.lang.marker) {
					t.Errorf("%s=%q, want marker %q", key, got, tt.lang.marker)
				}
			}
		})
	}
}

type i18nTestLang struct {
	code   Lang
	marker string
}
