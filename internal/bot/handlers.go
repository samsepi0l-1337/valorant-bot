package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/authweb"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
	qrcode "github.com/skip2/go-qrcode"
)

// qrAttachmentName is referenced by the embed as attachment://<name>.
const qrAttachmentName = "riot-qr.png"

// qrImageSize is the QR PNG edge length in pixels; large enough to scan from a
// desktop screen without dominating the ephemeral message.
const qrImageSize = 320
const mfaHintTTL = 15 * time.Minute

const (
	customIDAuthQR          = "auth:qr"
	customIDAuthPassword    = "auth:password"
	customIDAuthPWModal     = "auth:pw"
	customIDAuthCaptchaPref = "auth:captcha:"
	customIDAuthMFAPref     = "auth:mfa:"
	customIDAuthMFAOpenPref = "auth:mfaopen:"
)

// HandleAuth returns the dual auth chooser: Riot Mobile QR or Discord modal ID login.
func (h *Handlers) HandleAuth(ctx context.Context, discordUserID string, lang i18n.Lang) (Response, error) {
	_ = ctx
	_ = discordUserID
	return Response{
		Ephemeral: true,
		Content:   i18n.T(lang, "auth.choose"),
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label:    i18n.T(lang, "auth.choose.qr"),
						Style:    discordgo.PrimaryButton,
						CustomID: customIDAuthQR,
					},
					discordgo.Button{
						Label:    i18n.T(lang, "auth.choose.password"),
						Style:    discordgo.SecondaryButton,
						CustomID: customIDAuthPassword,
					},
				},
			},
		},
	}, nil
}

// HandleAuthQR starts a Riot Mobile QR login and returns the ephemeral message
// holding the QR image. The returned state is passed to WaitQRLogin.
func (h *Handlers) HandleAuthQR(ctx context.Context, discordUserID string, lang i18n.Lang) (Response, string, error) {
	if h.Auth == nil {
		return Response{}, "", fmt.Errorf("auth not configured")
	}
	scanURL, state, err := h.Auth.BeginQRAuth(ctx, discordUserID)
	if err != nil {
		return Response{}, "", err
	}
	png, err := qrcode.Encode(scanURL, qrcode.Medium, qrImageSize)
	if err != nil {
		return Response{}, "", err
	}
	return Response{
		Ephemeral: true,
		Content:   i18n.T(lang, "auth.prompt"),
		Files: []*discordgo.File{{
			Name:        qrAttachmentName,
			ContentType: "image/png",
			Reader:      bytes.NewReader(png),
		}},
		Embeds: []*discordgo.MessageEmbed{{
			Color: 0xFD4553,
			Image: &discordgo.MessageEmbedImage{URL: "attachment://" + qrAttachmentName},
		}},
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label: i18n.T(lang, "auth.button"),
						Style: discordgo.LinkButton,
						URL:   scanURL,
					},
				},
			},
		},
	}, state, nil
}

// PasswordLoginModal is step 1: Riot username/password only.
// Step 2 (email or authenticator OTP) opens after Riot challenges MFA.
func PasswordLoginModal(lang i18n.Lang) *discordgo.InteractionResponseData {
	return &discordgo.InteractionResponseData{
		CustomID: customIDAuthPWModal,
		Title:    i18n.T(lang, "auth.password.title"),
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:    "username",
						Label:       i18n.T(lang, "auth.password.username"),
						Style:       discordgo.TextInputShort,
						Placeholder: "Riot ID / email",
						Required:    true,
						MaxLength:   128,
					},
				},
			},
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:  "password",
						Label:     i18n.T(lang, "auth.password.password"),
						Style:     discordgo.TextInputShort,
						Required:  true,
						MinLength: 1,
						MaxLength: 128,
					},
				},
			},
		},
	}
}

// MFALoginModal is step 2: email or authenticator / Riot Mobile OTP.
func MFALoginModal(mfaState string, hint string, lang i18n.Lang) *discordgo.InteractionResponseData {
	label := i18n.T(lang, "auth.mfa.code")
	switch {
	case strings.Contains(hint, "@"), hint == "email":
		label = i18n.T(lang, "auth.mfa.code_email")
	case hint == "authenticator", hint == "otp", hint == "otpauth", hint == "riot_mobile", hint == "mobile":
		label = i18n.T(lang, "auth.mfa.code_app")
	}
	return &discordgo.InteractionResponseData{
		CustomID: customIDAuthMFAPref + mfaState,
		Title:    i18n.T(lang, "auth.mfa.title"),
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:    "code",
						Label:       label,
						Style:       discordgo.TextInputShort,
						Placeholder: "000000",
						Required:    true,
						MinLength:   6,
						MaxLength:   8,
					},
				},
			},
		},
	}
}

func mfaPrompt(lang i18n.Lang, hint string) string {
	switch {
	case strings.Contains(hint, "@"):
		return fmt.Sprintf(i18n.T(lang, "auth.mfa.prompt_email"), hint)
	case hint == "email":
		return i18n.T(lang, "auth.mfa.prompt_email_generic")
	case hint == "authenticator", hint == "otp", hint == "otpauth", hint == "riot_mobile", hint == "mobile":
		return i18n.T(lang, "auth.mfa.prompt_app")
	default:
		return i18n.T(lang, "auth.mfa.prompt")
	}
}

func (h *Handlers) rememberMFAHint(state, hint string) {
	if state == "" {
		return
	}
	h.mfaHintMu.Lock()
	if h.mfaHints == nil {
		h.mfaHints = make(map[string]string)
	}
	h.mfaHints[state] = hint
	h.mfaHintMu.Unlock()
	ctx, done, ok := h.beginLifecycleWorker(mfaHintTTL)
	if !ok {
		return
	}
	go func() {
		defer done()
		<-ctx.Done()
		h.clearMFAHint(state)
	}()
}

func (h *Handlers) mfaHintFor(state string) string {
	h.mfaHintMu.Lock()
	defer h.mfaHintMu.Unlock()
	if h.mfaHints == nil {
		return ""
	}
	return h.mfaHints[state]
}

func (h *Handlers) clearMFAHint(state string) {
	h.mfaHintMu.Lock()
	defer h.mfaHintMu.Unlock()
	if h.mfaHints != nil {
		delete(h.mfaHints, state)
	}
}

func mfaRetryComponents(mfaState string, lang i18n.Lang) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    i18n.T(lang, "auth.mfa.button"),
					Style:    discordgo.PrimaryButton,
					CustomID: customIDAuthMFAOpenPref + mfaState,
				},
			},
		},
	}
}

// HandlePasswordLogin is step 1: prepare the owner-bound CAPTCHA button after
// username/password. The button starts bot-host Chrome.
func (h *Handlers) HandlePasswordLogin(ctx context.Context, discordUserID, username, password string, lang i18n.Lang) (Response, string, error) {
	if h.Auth == nil {
		return Response{}, "", fmt.Errorf("auth not configured")
	}
	_, state, err := h.Auth.BeginPasswordLogin(ctx, discordUserID, username, password)
	if err != nil {
		errText := strings.ToLower(err.Error())
		if strings.Contains(errText, "chrome") || strings.Contains(errText, "chromium") {
			return Response{
				Ephemeral: true,
				Content:   i18n.T(lang, "auth.captcha.need_chrome"),
			}, "", nil
		}
		return Response{
			Ephemeral: true,
			Content:   fmt.Sprintf(i18n.T(lang, "auth.password.failed"), err),
		}, "", nil
	}
	resp := Response{
		Ephemeral: true,
		Content:   i18n.T(lang, "auth.captcha.prompt"),
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label:    i18n.T(lang, "auth.captcha.button"),
						Style:    discordgo.SecondaryButton,
						CustomID: customIDAuthCaptchaPref + state,
					},
				},
			},
		},
	}
	return resp, state, nil
}

// HandlePasswordCaptchaLaunch handles the Discord custom-ID button by opening
// Chrome on the bot host after validating the login owner.
func (h *Handlers) HandlePasswordCaptchaLaunch(ctx context.Context, state, discordUserID string, lang i18n.Lang) (Response, error) {
	resp, _, err := h.handlePasswordCaptchaLaunch(ctx, state, discordUserID, lang)
	return resp, err
}

// handlePasswordCaptchaLaunch returns whether Chrome was opened in addition to
// the localized response used by the Discord component handler.
func (h *Handlers) handlePasswordCaptchaLaunch(ctx context.Context, state, discordUserID string, lang i18n.Lang) (Response, bool, error) {
	if h.Auth == nil {
		return Response{}, false, fmt.Errorf("auth not configured")
	}
	err := h.Auth.LaunchPasswordCaptcha(ctx, state, discordUserID)
	if err == nil {
		return Response{Ephemeral: true, Content: i18n.T(lang, "auth.captcha.launched")}, true, nil
	}
	if errors.Is(err, authweb.ErrCaptchaOwner) {
		return Response{Ephemeral: true, Content: i18n.T(lang, "auth.captcha.denied")}, false, nil
	}
	errText := strings.ToLower(err.Error())
	if strings.Contains(errText, "chrome") || strings.Contains(errText, "chromium") {
		return Response{Ephemeral: true, Content: i18n.T(lang, "auth.captcha.need_chrome"), Components: []discordgo.MessageComponent{}}, false, nil
	}
	if strings.Contains(errText, "expired") {
		return Response{Ephemeral: true, Content: i18n.T(lang, "auth.captcha.expired"), Components: []discordgo.MessageComponent{}}, false, nil
	}
	return Response{Ephemeral: true, Content: fmt.Sprintf(i18n.T(lang, "auth.captcha.launch_failed"), err), Components: []discordgo.MessageComponent{}}, false, nil
}

// HandlePasswordCaptchaComplete builds the Discord reply after browser captcha finishes.
func (h *Handlers) HandlePasswordCaptchaComplete(display, mfaState, mfaHint string, err error, lang i18n.Lang) Response {
	if err != nil {
		msg := fmt.Sprintf(i18n.T(lang, "auth.password.failed"), err)
		errText := strings.ToLower(err.Error())
		switch {
		case strings.Contains(errText, "deadline exceeded"), strings.Contains(errText, "context canceled"):
			msg = i18n.T(lang, "auth.captcha.timeout")
		case strings.Contains(errText, "chrome"), strings.Contains(errText, "chromium"):
			msg = i18n.T(lang, "auth.captcha.need_chrome")
		case strings.Contains(errText, "unknown or expired captcha"),
			strings.Contains(errText, "captcha session expired"):
			msg = i18n.T(lang, "auth.captcha.expired")
		case strings.Contains(errText, "invalid_request"),
			strings.Contains(errText, "captcha"),
			errors.Is(err, riot.ErrPasswordCaptcha):
			msg = i18n.T(lang, "auth.captcha.rejected")
		}
		return Response{
			Ephemeral:  true,
			Content:    msg,
			Embeds:     []*discordgo.MessageEmbed{},
			Components: []discordgo.MessageComponent{},
		}
	}
	if mfaState != "" {
		h.rememberMFAHint(mfaState, mfaHint)
		return Response{
			Ephemeral:  true,
			Content:    mfaPrompt(lang, mfaHint),
			Embeds:     []*discordgo.MessageEmbed{},
			Components: mfaRetryComponents(mfaState, lang),
		}
	}
	return Response{
		Ephemeral:  true,
		Content:    fmt.Sprintf(i18n.T(lang, "auth.qr.done"), display),
		Embeds:     []*discordgo.MessageEmbed{},
		Components: []discordgo.MessageComponent{},
	}
}

func mfaTerminalMessage(lang i18n.Lang, err error) string {
	switch {
	case errors.Is(err, authweb.ErrMFAOwner):
		return i18n.T(lang, "auth.mfa.denied")
	case errors.Is(err, authweb.ErrMFAExpired):
		return i18n.T(lang, "auth.mfa.expired")
	default:
		return fmt.Sprintf(i18n.T(lang, "auth.mfa.failed"), err)
	}
}

// HandlePasswordMFA is step 2: submit email or authenticator OTP.
func (h *Handlers) HandlePasswordMFA(ctx context.Context, mfaState, discordUserID, code string, lang i18n.Lang) (Response, error) {
	if h.Auth == nil {
		return Response{}, fmt.Errorf("auth not configured")
	}
	display, err := h.Auth.CompletePasswordMFA(ctx, mfaState, discordUserID, code)
	if err != nil {
		if errors.Is(err, riot.ErrPasswordInvalidCode) {
			return Response{
				Ephemeral:  true,
				Content:    i18n.T(lang, "auth.mfa.invalid"),
				Components: mfaRetryComponents(mfaState, lang),
			}, nil
		}
		h.clearMFAHint(mfaState)
		return Response{
			Ephemeral:  true,
			Content:    mfaTerminalMessage(lang, err),
			Embeds:     []*discordgo.MessageEmbed{},
			Components: []discordgo.MessageComponent{},
		}, nil
	}
	h.clearMFAHint(mfaState)
	return Response{
		Ephemeral:  true,
		Content:    fmt.Sprintf(i18n.T(lang, "auth.qr.done"), display),
		Embeds:     []*discordgo.MessageEmbed{},
		Components: []discordgo.MessageComponent{},
	}, nil
}

// HandleAuthComplete renders the final message once a QR login settles.
func (h *Handlers) HandleAuthComplete(displayName string, err error, lang i18n.Lang) Response {
	resp := Response{
		Ephemeral:  true,
		Embeds:     []*discordgo.MessageEmbed{},
		Components: []discordgo.MessageComponent{},
	}
	switch {
	case err == nil:
		resp.Content = fmt.Sprintf(i18n.T(lang, "auth.qr.done"), displayName)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, riot.ErrQRExpired):
		resp.Content = i18n.T(lang, "auth.qr.timeout")
	default:
		resp.Content = fmt.Sprintf(i18n.T(lang, "auth.qr.failed"), err)
	}
	return resp
}

// HandleAccounts lists linked accounts for the user.
func (h *Handlers) HandleAccounts(discordUserID string, lang i18n.Lang) (Response, error) {
	if h.Accounts == nil {
		return Response{}, fmt.Errorf("accounts not configured")
	}
	accounts, err := h.Accounts.ListRiotAccountsByDiscord(discordUserID)
	if err != nil {
		return Response{}, err
	}
	if len(accounts) == 0 {
		return Response{Ephemeral: true, Content: i18n.T(lang, "accounts.empty")}, nil
	}
	var b strings.Builder
	b.WriteString(i18n.T(lang, "accounts.header") + "\n")
	for _, a := range accounts {
		fmt.Fprintf(&b, "• %s#%s (%s)\n", a.GameName, a.TagLine, riot.DisplayRegion(a.Region, string(lang)))
	}
	return Response{Ephemeral: true, Content: strings.TrimSpace(b.String())}, nil
}

// HandleUnlink removes a linked account by puuid or matching game name / name#tag.
func (h *Handlers) HandleUnlink(discordUserID, identifier string, lang i18n.Lang) (Response, error) {
	if h.Accounts == nil {
		return Response{}, fmt.Errorf("accounts not configured")
	}
	accounts, err := h.Accounts.ListRiotAccountsByDiscord(discordUserID)
	if err != nil {
		return Response{}, err
	}
	puuid, ok := resolveAccountPUUID(accounts, identifier)
	if !ok {
		return Response{}, fmt.Errorf(i18n.T(lang, "unlink.not_found"), identifier)
	}
	if err := h.Accounts.DeleteRiotAccount(discordUserID, puuid); err != nil {
		return Response{}, err
	}
	return Response{Ephemeral: true, Content: i18n.T(lang, "unlink.done")}, nil
}

// HandleShop fetches shops and builds embeds for each linked account.
func (h *Handlers) HandleShop(ctx context.Context, discordUserID string, lang i18n.Lang) (Response, error) {
	if h.Accounts == nil || h.Shops == nil {
		return Response{}, fmt.Errorf("shop not configured")
	}
	accounts, err := h.Accounts.ListRiotAccountsByDiscord(discordUserID)
	if err != nil {
		return Response{}, err
	}
	if len(accounts) == 0 {
		return Response{Ephemeral: true, Content: i18n.T(lang, "shop.empty")}, nil
	}
	shops, err := h.Shops.ShopsForUser(ctx, discordUserID, string(lang))
	if err != nil {
		return Response{}, err
	}
	h.ensureShopCache().put(discordUserID, shops, lang)
	return shopPageResponse(discordUserID, shops, 0, lang), nil
}

// HandleLanguage sets the user's bot reply language.
func (h *Handlers) HandleLanguage(discordUserID, langCode string) (Response, error) {
	if h.Lang == nil {
		return Response{}, fmt.Errorf("language not configured")
	}
	if langCode != "ko" && langCode != "en" {
		return Response{Ephemeral: true, Content: i18n.T(i18n.KO, "lang.invalid")}, nil
	}
	lang := i18n.Parse(langCode)
	if err := h.Lang.SetUserLanguage(discordUserID, string(lang)); err != nil {
		return Response{}, err
	}
	h.cacheUserLang(discordUserID, lang)
	msg := i18n.T(lang, "lang.set")
	if lang == i18n.EN {
		msg = i18n.T(lang, "lang.set_en")
	}
	return Response{Ephemeral: true, Content: msg}, nil
}

func resolveAccountPUUID(accounts []store.Account, identifier string) (string, bool) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", false
	}
	for _, a := range accounts {
		if a.PUUID == identifier {
			return a.PUUID, true
		}
	}
	lower := strings.ToLower(identifier)
	for _, a := range accounts {
		if strings.EqualFold(a.GameName, identifier) {
			return a.PUUID, true
		}
		combo := a.GameName + "#" + a.TagLine
		if strings.EqualFold(combo, identifier) {
			return a.PUUID, true
		}
		if strings.HasPrefix(strings.ToLower(a.PUUID), lower) {
			return a.PUUID, true
		}
	}
	return "", false
}

// FormatShopAccountLabel returns "Name#Tag (region)" for shop message headers.
func FormatShopAccountLabel(s AccountShop, lang i18n.Lang) string {
	account := fmt.Sprintf("%s#%s", s.GameName, s.TagLine)
	if s.Region != "" {
		account = fmt.Sprintf("%s (%s)", account, riot.DisplayRegion(s.Region, string(lang)))
	}
	return account
}

// BuildAccountPageEmbeds builds the /shop page for one account: one embed per
// skin (with image) so all four daily offers are visible. Pagination switches
// accounts, not skins. Account name/region belong in the message content above.
func BuildAccountPageEmbeds(s AccountShop, lang i18n.Lang) []*discordgo.MessageEmbed {
	switch {
	case s.Err != "":
		return []*discordgo.MessageEmbed{{
			Title:       i18n.T(lang, "shop.fail_title"),
			Description: s.Err,
			Color:       skins.DefaultEmbedColor,
		}}
	case len(s.Offers) == 0:
		return []*discordgo.MessageEmbed{{
			Description: i18n.T(lang, "shop.none"),
			Color:       skins.DefaultEmbedColor,
		}}
	}

	out := make([]*discordgo.MessageEmbed, 0, len(s.Offers))
	for _, o := range s.Offers {
		name := o.DisplayName
		if name == "" {
			name = o.SkinUUID
		}
		color := o.Color
		if color == 0 {
			color = skins.DefaultEmbedColor
		}
		embed := &discordgo.MessageEmbed{
			Title:       fmt.Sprintf("%s · %d VP", name, o.CostVP),
			Color:       color,
			Description: "\u200b",
		}
		if o.IconURL != "" {
			embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: o.IconURL}
		}
		out = append(out, embed)
	}
	return out
}

// BuildShopEmbeds converts account shops into compact Discord embeds: one
// embed per account. Used by the daily scheduler where many accounts may share
// one message (Discord caps at 10 embeds).
func BuildShopEmbeds(shops []AccountShop, lang i18n.Lang) []*discordgo.MessageEmbed {
	out := make([]*discordgo.MessageEmbed, 0, len(shops))
	for _, s := range shops {
		account := fmt.Sprintf("%s#%s", s.GameName, s.TagLine)
		if s.Region != "" {
			account = fmt.Sprintf("%s (%s)", account, riot.DisplayRegion(s.Region, string(lang)))
		}
		embed := &discordgo.MessageEmbed{
			Author: &discordgo.MessageEmbedAuthor{Name: account},
			Color:  skins.DefaultEmbedColor,
		}

		switch {
		case s.Err != "":
			embed.Title = i18n.T(lang, "shop.fail_title")
			embed.Description = s.Err
		case len(s.Offers) == 0:
			embed.Description = i18n.T(lang, "shop.none")
		default:
			embed.Fields = make([]*discordgo.MessageEmbedField, 0, len(s.Offers))
			for _, o := range s.Offers {
				name := o.DisplayName
				if name == "" {
					name = o.SkinUUID
				}
				embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
					Name:   name,
					Value:  fmt.Sprintf("%d VP", o.CostVP),
					Inline: true,
				})
				if embed.Thumbnail == nil && o.IconURL != "" {
					embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: o.IconURL}
				}
				if embed.Color == skins.DefaultEmbedColor && o.Color != 0 {
					embed.Color = o.Color
				}
			}
		}
		out = append(out, embed)
	}
	return out
}
