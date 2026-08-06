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
	"auth.choose":                "Riot 계정 연동 방식을 선택하세요.\n• **Riot Mobile**: QR 스캔 후 앱에서 승인\n• **아이디 로그인**: Discord 모달에 Riot 아이디/비밀번호 입력 (2차 인증도 모달)",
	"auth.choose.qr":             "Riot Mobile QR",
	"auth.choose.password":       "아이디 로그인",
	"auth.prompt":                "**Riot Mobile** 앱으로 아래 QR 코드를 스캔한 뒤 **로그인 승인**을 눌러주세요.\n휴대폰에서 Discord를 보고 있다면 아래 버튼을 눌러 바로 승인할 수 있습니다.\n승인하면 자동으로 연동됩니다. (약 3분 후 만료)",
	"auth.button":                "Riot Mobile로 열기",
	"auth.password.title":        "Riot 아이디 로그인",
	"auth.password.username":     "아이디 / 이메일",
	"auth.password.password":     "비밀번호",
	"auth.password.mfa_code":     "2차 인증 코드 (있으면)",
	"auth.password.failed":       "로그인 실패: %v\n아이디/비밀번호를 확인하세요. 캡차·2차 인증 때문에 막히면 Riot Mobile QR을 사용하세요.",
	"auth.mfa.title":             "2차 인증 코드",
	"auth.mfa.code":              "인증 코드",
	"auth.mfa.prompt":            "이메일 또는 **Riot Mobile** 앱에 표시된 6자리 인증 코드를 입력하세요.",
	"auth.mfa.prompt_email":      "**%s** 이메일 또는 **Riot Mobile** 앱의 인증 코드를 입력하세요.",
	"auth.mfa.prompt_app":        "**Riot Mobile** / OTP 앱에 표시된 6자리 인증 코드를 입력하세요.",
	"auth.mfa.invalid":           "인증 코드가 올바르지 않습니다. 이메일 또는 Riot Mobile 코드를 다시 입력하세요.",
	"auth.mfa.button":            "인증 코드 입력",
	"auth.qr.done":               "연동 완료: **%s**\n`/shop` 으로 오늘 상점을 확인하세요.",
	"auth.qr.timeout":            "QR 로그인 시간이 만료되었습니다. `/auth` 를 다시 실행해 주세요.",
	"auth.qr.failed":             "연동에 실패했습니다: %v\n`/auth` 를 다시 실행해 주세요.",
	"accounts.empty":       "연결된 계정이 없습니다. `/auth` 로 Riot 계정을 연결하세요.",
	"accounts.header":      "**연결된 계정**",
	"unlink.done":          "계정 연결 해제가 완료되었습니다.",
	"unlink.not_found":     "계정을 찾을 수 없습니다: %s",
	"shop.empty":           "인증된 계정이 없습니다. `/auth` 로 연결하세요.",
	"shop.fail_title":      "상점 조회 실패",
	"shop.none":            "오늘 상점에 스킨이 없습니다.",
	"shop.page":            "계정 %d / %d",
	"shop.prev":            "이전",
	"shop.next":            "다음",
	"shop.nav_denied":      "본인만 조정 가능합니다.",
	"shop.nav_expired":     "상점 결과가 만료되었습니다. `/shop` 을 다시 실행하세요.",
	"wishlist.not_found":         "해당 이름의 스킨을 찾을 수 없습니다.",
	"wishlist.added":             "위시리스트에 **%s** 을(를) 추가했습니다.",
	"wishlist.pick_add":          "**%d**개 스킨이 «%s» 검색과 일치합니다. 아래에서 정확한 스킨을 선택하세요.",
	"wishlist.pick_remove":       "여러 항목이 일치합니다. 아래에서 제거할 스킨을 선택하세요.",
	"wishlist.pick_placeholder":  "스킨 선택…",
	"wishlist.pick_denied":       "이 선택은 요청한 사용자만 사용할 수 있습니다.",
	"wishlist.empty":             "위시리스트가 비어 있습니다.",
	"wishlist.removed":           "위시리스트에서 **%s** 을(를) 제거했습니다.",
	"wishlist.remove_not_found":  "위시리스트에서 해당 항목을 찾을 수 없습니다.",
	"wishlist.header":            "**위시리스트**",
	"channel.set":                "이 채널(<#%s>)을 일일 상점 알림 채널로 설정했습니다.\n알림 시각: **%02d:00 KST** (`/channel time`으로 변경)",
	"channel.time_prompt":        "일일 상점 알림 시각을 선택하세요. (현재 **%02d:00 KST**)",
	"channel.time_placeholder":   "시각 선택 (KST)…",
	"channel.time_set":           "일일 상점 알림 시각을 **%02d:00 KST**로 설정했습니다.",
	"channel.time_need_set":      "아직 알림 채널이 없습니다. 알림을 받을 채널에서 `/channel set`을 실행하세요.",
	"channel.time_invalid":       "올바른 시각(0–23)을 선택하세요.",
	"channel.time_denied":        "이 서버의 설정만 변경할 수 있습니다.",
	"lang.set":                   "언어가 **한국어**로 설정되었습니다.",
	"lang.set_en":                "Language set to **English**.",
	"lang.invalid":               "지원 언어: `ko`, `en`",
	"error.prefix":               "오류: ",
}

var en = map[string]string{
	"auth.choose":                "Choose how to link your Riot account.\n• **Riot Mobile**: scan the QR and approve in the app\n• **ID login**: enter Riot username/password in a Discord modal (2FA via modal too)",
	"auth.choose.qr":             "Riot Mobile QR",
	"auth.choose.password":       "ID login",
	"auth.prompt":                "Scan the QR code below with the **Riot Mobile** app and approve the login.\nOn a phone, tap the button below to approve directly.\nLinking happens automatically once approved. (expires in ~3 minutes)",
	"auth.button":                "Open in Riot Mobile",
	"auth.password.title":        "Riot ID login",
	"auth.password.username":     "Username / email",
	"auth.password.password":     "Password",
	"auth.password.mfa_code":     "2FA code (if any)",
	"auth.password.failed":       "Login failed: %v\nCheck your username/password. If captcha or 2FA blocks this path, use Riot Mobile QR.",
	"auth.mfa.title":             "2FA verification",
	"auth.mfa.code":              "Verification code",
	"auth.mfa.prompt":            "Enter the 6-digit code from email or the **Riot Mobile** app.",
	"auth.mfa.prompt_email":      "Enter the code from **%s** or the **Riot Mobile** app.",
	"auth.mfa.prompt_app":        "Enter the 6-digit code from **Riot Mobile** / your authenticator app.",
	"auth.mfa.invalid":           "That code was incorrect. Try again with the email or Riot Mobile code.",
	"auth.mfa.button":            "Enter code",
	"auth.qr.done":               "Linked: **%s**\nUse `/shop` to see today's store.",
	"auth.qr.timeout":            "The QR login expired. Run `/auth` again.",
	"auth.qr.failed":             "Linking failed: %v\nRun `/auth` again.",
	"accounts.empty":       "No linked accounts. Use `/auth` to connect a Riot account.",
	"accounts.header":      "**Linked accounts**",
	"unlink.done":          "Account unlinked.",
	"unlink.not_found":     "Account not found: %s",
	"shop.empty":           "No linked accounts. Use `/auth` to connect.",
	"shop.fail_title":      "Shop fetch failed",
	"shop.none":            "No skins in today's shop.",
	"shop.page":            "Account %d / %d",
	"shop.prev":            "Previous",
	"shop.next":            "Next",
	"shop.nav_denied":      "Only you can control this.",
	"shop.nav_expired":     "Shop results expired. Run `/shop` again.",
	"wishlist.not_found":         "No skin matched that name.",
	"wishlist.added":             "Added **%s** to your wishlist.",
	"wishlist.pick_add":          "**%d** skins matched «%s». Pick the exact skin below.",
	"wishlist.pick_remove":       "Multiple matches. Pick the skin to remove below.",
	"wishlist.pick_placeholder":  "Select a skin…",
	"wishlist.pick_denied":       "Only the user who ran the command can use this menu.",
	"wishlist.empty":             "Your wishlist is empty.",
	"wishlist.removed":           "Removed **%s** from your wishlist.",
	"wishlist.remove_not_found":  "No matching wishlist item.",
	"wishlist.header":            "**Wishlist**",
	"channel.set":                "This channel (<#%s>) is now used for daily shop alerts.\nAlert hour: **%02d:00 KST** (change with `/channel time`)",
	"channel.time_prompt":        "Pick the daily shop alert hour. (current **%02d:00 KST**)",
	"channel.time_placeholder":   "Select hour (KST)…",
	"channel.time_set":           "Daily shop alert hour set to **%02d:00 KST**.",
	"channel.time_need_set":      "No alert channel yet. Run `/channel set` in the target channel.",
	"channel.time_invalid":       "Pick a valid hour (0–23).",
	"channel.time_denied":        "You can only change settings for this server.",
	"lang.set":                   "Language set to **한국어**.",
	"lang.set_en":                "Language set to **English**.",
	"lang.invalid":               "Supported languages: `ko`, `en`",
	"error.prefix":               "Error: ",
}
