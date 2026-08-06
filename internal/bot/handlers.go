package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
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

const (
	customIDAuthQR          = "auth:qr"
	customIDAuthPassword    = "auth:password"
	customIDAuthPWModal     = "auth:pw"
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

// PasswordLoginModal is the Discord modal for Riot username/password.
// Optional MFA code lets users submit email / Riot Mobile OTP in one step.
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
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:    "mfa_code",
						Label:       i18n.T(lang, "auth.password.mfa_code"),
						Style:       discordgo.TextInputShort,
						Placeholder: "000000",
						Required:    false,
						MinLength:   0,
						MaxLength:   8,
					},
				},
			},
		},
	}
}

// MFALoginModal asks for an email or Riot Mobile OTP after password MFA.
func MFALoginModal(mfaState string, hint string, lang i18n.Lang) *discordgo.InteractionResponseData {
	placeholder := "000000"
	if strings.Contains(hint, "@") {
		placeholder = hint
	}
	return &discordgo.InteractionResponseData{
		CustomID: customIDAuthMFAPref + mfaState,
		Title:    i18n.T(lang, "auth.mfa.title"),
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:    "code",
						Label:       i18n.T(lang, "auth.mfa.code"),
						Style:       discordgo.TextInputShort,
						Placeholder: placeholder,
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
	case hint == "authenticator", hint == "otp", hint == "otpauth", hint == "riot_mobile", hint == "mobile":
		return i18n.T(lang, "auth.mfa.prompt_app")
	default:
		return i18n.T(lang, "auth.mfa.prompt")
	}
}

// HandlePasswordLogin links an account from Discord modal credentials.
// mfaCode is optional; when Riot challenges MFA and a code was provided, it is submitted immediately.
func (h *Handlers) HandlePasswordLogin(ctx context.Context, discordUserID, username, password, mfaCode string, lang i18n.Lang) (Response, error) {
	if h.Auth == nil {
		return Response{}, fmt.Errorf("auth not configured")
	}
	display, mfaState, mfaHint, err := h.Auth.LoginWithPassword(ctx, discordUserID, username, password)
	if err != nil {
		return Response{
			Ephemeral: true,
			Content:   fmt.Sprintf(i18n.T(lang, "auth.password.failed"), err),
		}, nil
	}
	if mfaState != "" {
		if code := strings.TrimSpace(mfaCode); code != "" {
			return h.HandlePasswordMFA(ctx, mfaState, code, lang)
		}
		return Response{
			Ephemeral: true,
			Content:   mfaPrompt(lang, mfaHint),
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.Button{
							Label:    i18n.T(lang, "auth.mfa.button"),
							Style:    discordgo.PrimaryButton,
							CustomID: customIDAuthMFAOpenPref + mfaState,
						},
					},
				},
			},
		}, nil
	}
	return Response{
		Ephemeral:  true,
		Content:    fmt.Sprintf(i18n.T(lang, "auth.qr.done"), display),
		Embeds:     []*discordgo.MessageEmbed{},
		Components: []discordgo.MessageComponent{},
	}, nil
}

// HandlePasswordMFA completes MFA for a pending password login.
func (h *Handlers) HandlePasswordMFA(ctx context.Context, mfaState, code string, lang i18n.Lang) (Response, error) {
	if h.Auth == nil {
		return Response{}, fmt.Errorf("auth not configured")
	}
	display, err := h.Auth.CompletePasswordMFA(ctx, mfaState, code)
	if err != nil {
		msg := fmt.Sprintf(i18n.T(lang, "auth.password.failed"), err)
		if strings.Contains(strings.ToLower(err.Error()), "invalid multifactor") ||
			strings.Contains(strings.ToLower(err.Error()), "invalid_code") {
			msg = i18n.T(lang, "auth.mfa.invalid")
		}
		return Response{
			Ephemeral: true,
			Content:   msg,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.Button{
							Label:    i18n.T(lang, "auth.mfa.button"),
							Style:    discordgo.PrimaryButton,
							CustomID: customIDAuthMFAOpenPref + mfaState,
						},
					},
				},
			},
		}, nil
	}
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
