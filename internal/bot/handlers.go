package bot

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

// HandleAuth starts OAuth and returns an ephemeral message with the login URL.
func (h *Handlers) HandleAuth(discordUserID string, lang i18n.Lang) (Response, error) {
	if h.Auth == nil {
		return Response{}, fmt.Errorf("auth not configured")
	}
	loginURL, _, err := h.Auth.BeginAuth(discordUserID)
	if err != nil {
		return Response{}, err
	}
	return Response{
		Ephemeral: true,
		Content:   i18n.T(lang, "auth.prompt"),
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label: i18n.T(lang, "auth.button"),
						Style: discordgo.LinkButton,
						URL:   loginURL,
					},
				},
			},
		},
	}, nil
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
		fmt.Fprintf(&b, "• %s#%s (%s)\n", a.GameName, a.TagLine, a.Region)
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
	return Response{Embeds: BuildShopEmbeds(shops, lang)}, nil
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

// BuildShopEmbeds converts account shops into compact Discord embeds.
// One embed per skin with Thumbnail (not full-width Image) so ~2 accounts fit on one screen.
func BuildShopEmbeds(shops []AccountShop, lang i18n.Lang) []*discordgo.MessageEmbed {
	out := make([]*discordgo.MessageEmbed, 0)
	for _, s := range shops {
		account := fmt.Sprintf("%s#%s", s.GameName, s.TagLine)
		if s.Region != "" {
			account = fmt.Sprintf("%s (%s)", account, s.Region)
		}

		if s.Err != "" {
			out = append(out, &discordgo.MessageEmbed{
				Author:      &discordgo.MessageEmbedAuthor{Name: account},
				Title:       i18n.T(lang, "shop.fail_title"),
				Description: s.Err,
				Color:       0xFD4553,
			})
			continue
		}
		if len(s.Offers) == 0 {
			out = append(out, &discordgo.MessageEmbed{
				Author:      &discordgo.MessageEmbedAuthor{Name: account},
				Description: i18n.T(lang, "shop.none"),
				Color:       0xFD4553,
			})
			continue
		}
		for i, o := range s.Offers {
			name := o.DisplayName
			if name == "" {
				name = o.SkinUUID
			}
			embed := &discordgo.MessageEmbed{
				// Single line: name + price (avoids extra description block height).
				Title: fmt.Sprintf("%s · %d VP", name, o.CostVP),
				Color: 0xFD4553,
			}
			if i == 0 {
				embed.Author = &discordgo.MessageEmbedAuthor{Name: account}
			}
			if o.IconURL != "" {
				embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: o.IconURL}
			}
			out = append(out, embed)
		}
	}
	return out
}
