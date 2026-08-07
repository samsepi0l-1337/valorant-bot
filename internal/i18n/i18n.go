package i18n

// Lang is a bot UI language code.
type Lang string

const (
	KO Lang = "ko"
	EN Lang = "en"
)

// Parse returns a known language or KO as default.
func Parse(s string) Lang {
	switch s {
	case "en", "EN", "english":
		return EN
	case "ko", "KO", "korean", "kr":
		return KO
	default:
		return KO
	}
}

// T returns a localized string for key.
func T(lang Lang, key string) string {
	if lang == EN {
		if s, ok := en[key]; ok {
			return s
		}
	}
	if s, ok := ko[key]; ok {
		return s
	}
	if s, ok := en[key]; ok {
		return s
	}
	return key
}

var ko = map[string]string{
	"auth.choose":                   "Riot 계정 연동 방식을 선택하세요.\n• **Riot Mobile**: QR 스캔 후 앱에서 승인\n• **아이디 로그인**: ① Discord에 아이디/비밀번호 → ② 봇 PC Chrome에서 「로봇이 아닙니다」 → ③(있을 때만) 2FA 코드",
	"auth.choose.qr":                "Riot Mobile QR",
	"auth.choose.password":          "아이디 로그인",
	"auth.prompt":                   "**Riot Mobile** 앱으로 아래 QR 코드를 스캔한 뒤 **로그인 승인**을 눌러주세요.\n휴대폰에서 Discord를 보고 있다면 아래 버튼을 눌러 바로 승인할 수 있습니다.\n승인하면 자동으로 연동됩니다. (약 3분 후 만료)",
	"auth.button":                   "Riot Mobile로 열기",
	"auth.password.title":           "1단계 · 아이디/비밀번호",
	"auth.password.username":        "아이디 / 이메일",
	"auth.password.password":        "비밀번호",
	"auth.password.failed":          "로그인 실패: %v\n아이디/비밀번호를 확인한 뒤 다시 시도하세요.",
	"auth.captcha.prompt":           "봇이 실행 중인 이 Mac/PC에서 Chrome 캡차 창을 여는 중입니다.\n그 창에서 **「로봇이 아닙니다」**만 체크하세요.\n창이 보이지 않으면 아래 **캡차 창 다시 열기**를 누르세요.",
	"auth.captcha.button":           "캡차 창 다시 열기",
	"auth.captcha.launched":         "봇 PC에서 Chrome 캡차 창을 다시 열었습니다.",
	"auth.captcha.denied":           "이 캡차 창은 로그인 요청을 시작한 본인만 다시 열 수 있습니다.",
	"auth.captcha.launch_failed":    "캡차 창을 열지 못했습니다: %v\n`/auth`를 다시 실행하거나 Riot Mobile QR을 사용하세요.",
	"auth.captcha.need_chrome":      "아이디 로그인 캡차는 **봇이 실행 중인 PC의 Google Chrome/Chromium**이 필요합니다.\nChrome을 설치한 뒤 다시 `/auth` 하거나, **Riot Mobile QR**을 사용하세요.",
	"auth.captcha.rejected":         "캡차가 Riot에서 거절되었습니다. `/auth` 를 다시 실행해 주세요.\n터널 도메인이나 일반 브라우저에서 푼 캡차는 사용할 수 없습니다.",
	"auth.captcha.expired":          "캡차 세션이 만료되었습니다. `/auth` 를 다시 실행해 주세요.",
	"auth.captcha.timeout":          "캡차 대기 시간이 끝났습니다. `/auth` 를 다시 실행해 주세요.",
	"auth.mfa.title":                "2단계 · 인증 코드",
	"auth.mfa.code":                 "6자리 인증 코드",
	"auth.mfa.code_email":           "이메일 인증 코드",
	"auth.mfa.code_app":             "2FA 앱 / Riot Mobile 코드",
	"auth.mfa.prompt":               "캡차 완료.\n이제 이메일 또는 **2FA 앱 / Riot Mobile**에 표시된 6자리 코드를 입력하세요.",
	"auth.mfa.prompt_email":         "캡차 완료.\n**%s** 으로 보낸 인증 코드를 확인한 뒤 아래 버튼으로 입력하세요.",
	"auth.mfa.prompt_email_generic": "캡차 완료.\n이메일로 온 인증 코드를 확인한 뒤 아래 버튼으로 입력하세요.",
	"auth.mfa.prompt_app":           "캡차 완료.\n**2FA 앱** 또는 **Riot Mobile**에 표시된 6자리 코드를 아래 버튼으로 입력하세요.",
	"auth.mfa.invalid":              "인증 코드가 올바르지 않습니다. 새 코드를 확인한 뒤 다시 입력하세요.",
	"auth.mfa.failed":               "2차 인증 실패: %v\n아래 버튼으로 코드를 다시 입력하세요.",
	"auth.mfa.button":               "인증 코드 입력",
	"auth.qr.done":                  "연동 완료: **%s**\n`/shop` 으로 오늘 상점을 확인하세요.",
	"auth.qr.timeout":               "QR 로그인 시간이 만료되었습니다. `/auth` 를 다시 실행해 주세요.",
	"auth.qr.failed":                "연동에 실패했습니다: %v\n`/auth` 를 다시 실행해 주세요.",
	"accounts.empty":                "연결된 계정이 없습니다. `/auth` 로 Riot 계정을 연결하세요.",
	"accounts.header":               "**연결된 계정**",
	"unlink.done":                   "계정 연결 해제가 완료되었습니다.",
	"unlink.not_found":              "계정을 찾을 수 없습니다: %s",
	"shop.empty":                    "인증된 계정이 없습니다. `/auth` 로 연결하세요.",
	"shop.fail_title":               "상점 조회 실패",
	"shop.none":                     "오늘 상점에 스킨이 없습니다.",
	"shop.page":                     "계정 %d / %d",
	"shop.prev":                     "이전",
	"shop.next":                     "다음",
	"shop.nav_denied":               "본인만 조정 가능합니다.",
	"shop.nav_expired":              "상점 결과가 만료되었습니다. `/shop` 을 다시 실행하세요.",
	"wishlist.not_found":            "해당 이름의 스킨을 찾을 수 없습니다.",
	"wishlist.added":                "위시리스트에 **%s** 을(를) 추가했습니다.",
	"wishlist.pick_add":             "**%d**개 스킨이 «%s» 검색과 일치합니다. 아래에서 정확한 스킨을 선택하세요.",
	"wishlist.pick_remove":          "여러 항목이 일치합니다. 아래에서 제거할 스킨을 선택하세요.",
	"wishlist.pick_placeholder":     "스킨 선택…",
	"wishlist.pick_denied":          "이 선택은 요청한 사용자만 사용할 수 있습니다.",
	"wishlist.empty":                "위시리스트가 비어 있습니다.",
	"wishlist.removed":              "위시리스트에서 **%s** 을(를) 제거했습니다.",
	"wishlist.remove_not_found":     "위시리스트에서 해당 항목을 찾을 수 없습니다.",
	"wishlist.header":               "**위시리스트**",
	"channel.set":                   "이 채널(<#%s>)을 일일 상점 알림 채널로 설정했습니다.\n알림 시각: **%02d:00 KST** (`/channel time`으로 변경)",
	"channel.time_prompt":           "일일 상점 알림 시각을 선택하세요. (현재 **%02d:00 KST**)",
	"channel.time_placeholder":      "시각 선택 (KST)…",
	"channel.time_set":              "일일 상점 알림 시각을 **%02d:00 KST**로 설정했습니다.",
	"channel.time_need_set":         "아직 알림 채널이 없습니다. 알림을 받을 채널에서 `/channel set`을 실행하세요.",
	"channel.time_invalid":          "올바른 시각(0–23)을 선택하세요.",
	"channel.time_denied":           "이 서버의 설정만 변경할 수 있습니다.",
	"lang.set":                      "언어가 **한국어**로 설정되었습니다.",
	"lang.set_en":                   "Language set to **English**.",
	"lang.invalid":                  "지원 언어: `ko`, `en`",
	"error.prefix":                  "오류: ",
}

var en = map[string]string{
	"auth.choose":                   "Choose how to link your Riot account.\n• **Riot Mobile**: scan the QR and approve in the app\n• **ID login**: ① Discord username/password → ② bot-PC Chrome 「I'm not a robot」 → ③ 2FA code only if required",
	"auth.choose.qr":                "Riot Mobile QR",
	"auth.choose.password":          "ID login",
	"auth.prompt":                   "Scan the QR code below with the **Riot Mobile** app and approve the login.\nOn a phone, tap the button below to approve directly.\nLinking happens automatically once approved. (expires in ~3 minutes)",
	"auth.button":                   "Open in Riot Mobile",
	"auth.password.title":           "Step 1 · Username/password",
	"auth.password.username":        "Username / email",
	"auth.password.password":        "Password",
	"auth.password.failed":          "Login failed: %v\nCheck your username/password and try again.",
	"auth.captcha.prompt":           "Opening a Chrome captcha window on the bot Mac/PC.\nCheck **I'm not a robot** in that window.\nIf no window appears, tap **Reopen captcha window** below.",
	"auth.captcha.button":           "Reopen captcha window",
	"auth.captcha.launched":         "Reopened the Chrome captcha window on the bot machine.",
	"auth.captcha.denied":           "Only the user who started this login can reopen its captcha window.",
	"auth.captcha.launch_failed":    "Could not open the captcha window: %v\nRun `/auth` again or use Riot Mobile QR.",
	"auth.captcha.need_chrome":      "ID-login captcha needs **Google Chrome/Chromium on the bot machine**.\nInstall Chrome and retry `/auth`, or use **Riot Mobile QR**.",
	"auth.captcha.rejected":         "Riot rejected the captcha. Run `/auth` again.\nTokens solved on a tunnel domain or regular browser cannot be used.",
	"auth.captcha.expired":          "Captcha session expired. Run `/auth` again.",
	"auth.captcha.timeout":          "Captcha wait timed out. Run `/auth` again.",
	"auth.mfa.title":                "Step 2 · Verification code",
	"auth.mfa.code":                 "6-digit code",
	"auth.mfa.code_email":           "Email verification code",
	"auth.mfa.code_app":             "Authenticator / Riot Mobile code",
	"auth.mfa.prompt":               "Captcha done.\nEnter the 6-digit code from email or your **authenticator / Riot Mobile** app.",
	"auth.mfa.prompt_email":         "Captcha done.\nCheck **%s** for the code, then tap the button below to enter it.",
	"auth.mfa.prompt_email_generic": "Captcha done.\nCheck your email for the code, then tap the button below to enter it.",
	"auth.mfa.prompt_app":           "Captcha done.\nOpen your **authenticator** or **Riot Mobile** app for the 6-digit code, then tap the button below.",
	"auth.mfa.invalid":              "That code was incorrect. Check for a new code and try again.",
	"auth.mfa.failed":               "2FA failed: %v\nTap below to retry the code.",
	"auth.mfa.button":               "Enter code",
	"auth.qr.done":                  "Linked: **%s**\nUse `/shop` to see today's store.",
	"auth.qr.timeout":               "The QR login expired. Run `/auth` again.",
	"auth.qr.failed":                "Linking failed: %v\nRun `/auth` again.",
	"accounts.empty":                "No linked accounts. Use `/auth` to connect a Riot account.",
	"accounts.header":               "**Linked accounts**",
	"unlink.done":                   "Account unlinked.",
	"unlink.not_found":              "Account not found: %s",
	"shop.empty":                    "No linked accounts. Use `/auth` to connect.",
	"shop.fail_title":               "Shop fetch failed",
	"shop.none":                     "No skins in today's shop.",
	"shop.page":                     "Account %d / %d",
	"shop.prev":                     "Previous",
	"shop.next":                     "Next",
	"shop.nav_denied":               "Only you can control this.",
	"shop.nav_expired":              "Shop results expired. Run `/shop` again.",
	"wishlist.not_found":            "No skin matched that name.",
	"wishlist.added":                "Added **%s** to your wishlist.",
	"wishlist.pick_add":             "**%d** skins matched «%s». Pick the exact skin below.",
	"wishlist.pick_remove":          "Multiple matches. Pick the skin to remove below.",
	"wishlist.pick_placeholder":     "Select a skin…",
	"wishlist.pick_denied":          "Only the user who ran the command can use this menu.",
	"wishlist.empty":                "Your wishlist is empty.",
	"wishlist.removed":              "Removed **%s** from your wishlist.",
	"wishlist.remove_not_found":     "No matching wishlist item.",
	"wishlist.header":               "**Wishlist**",
	"channel.set":                   "This channel (<#%s>) is now used for daily shop alerts.\nAlert hour: **%02d:00 KST** (change with `/channel time`)",
	"channel.time_prompt":           "Pick the daily shop alert hour. (current **%02d:00 KST**)",
	"channel.time_placeholder":      "Select hour (KST)…",
	"channel.time_set":              "Daily shop alert hour set to **%02d:00 KST**.",
	"channel.time_need_set":         "No alert channel yet. Run `/channel set` in the target channel.",
	"channel.time_invalid":          "Pick a valid hour (0–23).",
	"channel.time_denied":           "You can only change settings for this server.",
	"lang.set":                      "Language set to **한국어**.",
	"lang.set_en":                   "Language set to **English**.",
	"lang.invalid":                  "Supported languages: `ko`, `en`",
	"error.prefix":                  "Error: ",
}
