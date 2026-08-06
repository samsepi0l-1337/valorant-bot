package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
)

var errUnknownSubcommand = errors.New("unknown subcommand")

func loc(m map[discordgo.Locale]string) *map[discordgo.Locale]string { return &m }

// Commands returns slash command definitions for guild registration only.
func Commands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:                     "auth",
			Description:              "Link a Riot account (Mobile QR or ID/password modal)",
			DescriptionLocalizations: loc(map[discordgo.Locale]string{discordgo.Korean: "Riot 계정 연동 (Mobile QR 또는 아이디/비밀번호)"}),
		},
		{
			Name:                     "accounts",
			Description:              "List your linked Riot accounts",
			DescriptionLocalizations: loc(map[discordgo.Locale]string{discordgo.Korean: "연동된 Riot 계정 목록"}),
		},
		{
			Name:                     "unlink",
			Description:              "Remove a linked Riot account",
			DescriptionLocalizations: loc(map[discordgo.Locale]string{discordgo.Korean: "Riot 계정 연동 해제"}),
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:                     discordgo.ApplicationCommandOptionString,
					Name:                     "puuid",
					Description:              "Account PUUID or Riot ID (Name or Name#Tag)",
					DescriptionLocalizations: map[discordgo.Locale]string{discordgo.Korean: "PUUID 또는 Riot ID"},
					Required:                 true,
				},
			},
		},
		{
			Name:                     "shop",
			Description:              "Show today's Valorant store for your linked accounts",
			DescriptionLocalizations: loc(map[discordgo.Locale]string{discordgo.Korean: "오늘 상점 스킨 확인"}),
		},
		{
			Name:                     "wishlist",
			Description:              "Manage your skin wishlist",
			DescriptionLocalizations: loc(map[discordgo.Locale]string{discordgo.Korean: "스킨 위시리스트 관리"}),
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:                     discordgo.ApplicationCommandOptionSubCommand,
					Name:                     "add",
					Description:              "Add a skin by name search",
					DescriptionLocalizations: map[discordgo.Locale]string{discordgo.Korean: "스킨 이름으로 추가"},
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:                     discordgo.ApplicationCommandOptionString,
							Name:                     "query",
							Description:              "Skin name to search",
							DescriptionLocalizations: map[discordgo.Locale]string{discordgo.Korean: "검색할 스킨 이름"},
							Required:                 true,
						},
					},
				},
				{
					Type:                     discordgo.ApplicationCommandOptionSubCommand,
					Name:                     "remove",
					Description:              "Remove a skin by name or UUID",
					DescriptionLocalizations: map[discordgo.Locale]string{discordgo.Korean: "이름 또는 UUID로 제거"},
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:                     discordgo.ApplicationCommandOptionString,
							Name:                     "query",
							Description:              "Skin name fragment or UUID",
							DescriptionLocalizations: map[discordgo.Locale]string{discordgo.Korean: "스킨 이름 또는 UUID"},
							Required:                 true,
						},
					},
				},
				{
					Type:                     discordgo.ApplicationCommandOptionSubCommand,
					Name:                     "list",
					Description:              "List your wishlist",
					DescriptionLocalizations: map[discordgo.Locale]string{discordgo.Korean: "위시리스트 보기"},
				},
			},
		},
		{
			Name:                     "channel",
			Description:              "Configure guild notification channel",
			DescriptionLocalizations: loc(map[discordgo.Locale]string{discordgo.Korean: "길드 알림 채널 설정"}),
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:                     discordgo.ApplicationCommandOptionSubCommand,
					Name:                     "set",
					Description:              "Set this channel for daily store posts",
					DescriptionLocalizations: map[discordgo.Locale]string{discordgo.Korean: "현재 채널을 일일 상점 알림으로 지정"},
				},
				{
					Type:                     discordgo.ApplicationCommandOptionSubCommand,
					Name:                     "time",
					Description:              "Pick daily shop alert hour (KST)",
					DescriptionLocalizations: map[discordgo.Locale]string{discordgo.Korean: "일일 상점 알림 시각 선택 (KST)"},
				},
			},
		},
		{
			Name:                     "language",
			Description:              "Set bot reply language",
			DescriptionLocalizations: loc(map[discordgo.Locale]string{discordgo.Korean: "봇 응답 언어 설정"}),
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:                     discordgo.ApplicationCommandOptionString,
					Name:                     "lang",
					Description:              "Language code",
					DescriptionLocalizations: map[discordgo.Locale]string{discordgo.Korean: "언어 코드"},
					Required:                 true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "한국어 (ko)", Value: "ko"},
						{Name: "English (en)", Value: "en"},
					},
				},
			},
		},
	}
}

// RegisterHandlers attaches the interaction handler to the session.
func RegisterHandlers(s *discordgo.Session, h *Handlers) {
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		h.OnInteraction(s, i)
	})
}

// OnInteraction dispatches application commands and component interactions.
func (h *Handlers) OnInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		h.onAppCommand(s, i)
	case discordgo.InteractionMessageComponent:
		h.onComponent(s, i)
	case discordgo.InteractionModalSubmit:
		h.onModal(s, i)
	}
}

func (h *Handlers) onComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	userID := interactionUserID(i)
	lang := h.userLang(userID)
	log.Printf("interaction: component %s user=%s", data.CustomID, userID)

	if data.CustomID == customIDAuthPassword {
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseModal,
			Data: PasswordLoginModal(lang),
		}); err != nil {
			log.Printf("interaction: password modal: %v", err)
		}
		return
	}
	if strings.HasPrefix(data.CustomID, customIDAuthMFAOpenPref) {
		mfaState := strings.TrimPrefix(data.CustomID, customIDAuthMFAOpenPref)
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseModal,
			Data: MFALoginModal(mfaState, "", lang),
		}); err != nil {
			log.Printf("interaction: mfa modal: %v", err)
		}
		return
	}

	var (
		resp           Response
		err            error
		keepComponents bool
		qrState        string
	)
	switch {
	case data.CustomID == customIDAuthQR:
		resp, qrState, err = h.HandleAuthQR(context.Background(), userID, lang)
		keepComponents = true
	case strings.HasPrefix(data.CustomID, customIDShopPagePrefix):
		owner, page, noop, ok := parseShopPageCustomID(data.CustomID)
		if !ok {
			err = fmt.Errorf("invalid shop navigation")
			break
		}
		if noop {
			return
		}
		resp, err = h.HandleShopNav(owner, page, userID, lang)
		keepComponents = true
	case strings.HasPrefix(data.CustomID, customIDWishlistAddPrefix):
		owner := strings.TrimPrefix(data.CustomID, customIDWishlistAddPrefix)
		if owner != userID {
			err = fmt.Errorf("%s", i18n.T(lang, "wishlist.pick_denied"))
			break
		}
		if len(data.Values) == 0 {
			err = fmt.Errorf("no selection")
			break
		}
		resp, err = h.HandleWishlistSelectAdd(userID, data.Values[0], lang)
	case strings.HasPrefix(data.CustomID, customIDWishlistRemovePrefix):
		owner := strings.TrimPrefix(data.CustomID, customIDWishlistRemovePrefix)
		if owner != userID {
			err = fmt.Errorf("%s", i18n.T(lang, "wishlist.pick_denied"))
			break
		}
		if len(data.Values) == 0 {
			err = fmt.Errorf("no selection")
			break
		}
		resp, err = h.HandleWishlistSelectRemove(userID, data.Values[0], lang)
	case strings.HasPrefix(data.CustomID, customIDChannelTimePrefix):
		guildID := strings.TrimPrefix(data.CustomID, customIDChannelTimePrefix)
		if i.GuildID != "" && i.GuildID != guildID {
			err = fmt.Errorf("%s", i18n.T(lang, "channel.time_denied"))
			break
		}
		if len(data.Values) == 0 {
			err = fmt.Errorf("no selection")
			break
		}
		resp, err = h.HandleChannelTimeSelect(guildID, data.Values[0], lang)
	default:
		log.Printf("interaction: ignoring component %q", data.CustomID)
		return
	}

	if err != nil {
		log.Printf("interaction: component error: %v", err)
		_ = updateComponentMessage(s, i, Response{Content: i18n.T(lang, "error.prefix") + err.Error()})
		return
	}
	// Ephemeral replies (e.g. shop nav denied) go only to the clicker and
	// must not replace the public component message.
	if resp.Ephemeral {
		if rerr := respondEphemeralEmbed(s, i, resp); rerr != nil {
			log.Printf("interaction: ephemeral component reply failed: %v", rerr)
		}
		return
	}
	if !keepComponents {
		resp.Components = []discordgo.MessageComponent{}
	}
	if data.CustomID == customIDAuthQR {
		if rerr := updateComponentMessageWithFiles(s, i, resp); rerr != nil {
			log.Printf("interaction: qr component update failed: %v", rerr)
		}
		if qrState != "" {
			go h.watchQRLogin(s, i, qrState, lang)
		}
		return
	}
	if rerr := updateComponentMessage(s, i, resp); rerr != nil {
		log.Printf("interaction: component update failed: %v", rerr)
	}
}

func (h *Handlers) onModal(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()
	userID := interactionUserID(i)
	lang := h.userLang(userID)
	log.Printf("interaction: modal %s user=%s", data.CustomID, userID)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	switch {
	case data.CustomID == customIDAuthPWModal:
		resp, err := h.HandlePasswordLogin(ctx, userID, modalValue(data, "username"), modalValue(data, "password"), modalValue(data, "mfa_code"), lang)
		if err != nil {
			_ = respondEphemeral(s, i, i18n.T(lang, "error.prefix")+err.Error())
			return
		}
		_ = respondEphemeralWithComponents(s, i, resp)
	case strings.HasPrefix(data.CustomID, customIDAuthMFAPref):
		mfaState := strings.TrimPrefix(data.CustomID, customIDAuthMFAPref)
		resp, err := h.HandlePasswordMFA(ctx, mfaState, modalValue(data, "code"), lang)
		if err != nil {
			_ = respondEphemeral(s, i, i18n.T(lang, "error.prefix")+err.Error())
			return
		}
		_ = respondEphemeralWithComponents(s, i, resp)
	default:
		log.Printf("interaction: ignoring modal %q", data.CustomID)
	}
}

func modalValue(data discordgo.ModalSubmitInteractionData, customID string) string {
	for _, row := range data.Components {
		actions, ok := row.(*discordgo.ActionsRow)
		if !ok {
			if ar, ok2 := row.(discordgo.ActionsRow); ok2 {
				for _, c := range ar.Components {
					if ti, ok := c.(*discordgo.TextInput); ok && ti.CustomID == customID {
						return ti.Value
					}
					if ti, ok := c.(discordgo.TextInput); ok && ti.CustomID == customID {
						return ti.Value
					}
				}
			}
			continue
		}
		for _, c := range actions.Components {
			if ti, ok := c.(*discordgo.TextInput); ok && ti.CustomID == customID {
				return ti.Value
			}
			if ti, ok := c.(discordgo.TextInput); ok && ti.CustomID == customID {
				return ti.Value
			}
		}
	}
	return ""
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func respondEphemeralWithComponents(s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) error {
	components := resp.Components
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:    resp.Content,
			Components: components,
			Flags:      discordgo.MessageFlagsEphemeral,
		},
	})
}

func respondEphemeralEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: resp.Content,
			Embeds:  resp.Embeds,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func updateComponentMessage(s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) error {
	var embeds *[]*discordgo.MessageEmbed
	if resp.Embeds != nil {
		embeds = &resp.Embeds
	}
	components := resp.Components
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	content := resp.Content
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    content,
			Embeds:     derefEmbeds(embeds),
			Components: components,
		},
	})
}

func updateComponentMessageWithFiles(s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) error {
	components := resp.Components
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    resp.Content,
			Embeds:     resp.Embeds,
			Components: components,
			Files:      resp.Files,
		},
	})
}

func derefEmbeds(p *[]*discordgo.MessageEmbed) []*discordgo.MessageEmbed {
	if p == nil {
		return nil
	}
	return *p
}

func (h *Handlers) onAppCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	userID := interactionUserID(i)
	log.Printf("interaction: /%s user=%s guild=%s", data.Name, userID, i.GuildID)

	// ACK within Discord's 3s window before any DB/network work.
	ephemeral := commandEphemeral(data.Name)
	if err := deferInteraction(s, i, ephemeral); err != nil {
		log.Printf("interaction: defer /%s failed: %v", data.Name, err)
		return
	}

	lang := h.userLang(userID)

	var (
		resp Response
		err  error
	)
	ctx := context.Background()

	switch data.Name {
	case "auth":
		resp, err = h.HandleAuth(ctx, userID, lang)
	case "accounts":
		resp, err = h.HandleAccounts(userID, lang)
	case "unlink":
		identifier := optionString(data.Options, "puuid")
		resp, err = h.HandleUnlink(userID, identifier, lang)
	case "shop":
		shopCtx, cancel := context.WithTimeout(ctx, shopTimeout)
		defer cancel()
		resp, err = h.HandleShop(shopCtx, userID, lang)
	case "wishlist":
		sub := subCommandName(data.Options)
		switch sub {
		case "add":
			resp, err = h.HandleWishlistAdd(userID, optionString(subCommandOptions(data.Options), "query"), lang)
		case "remove":
			resp, err = h.HandleWishlistRemove(userID, optionString(subCommandOptions(data.Options), "query"), lang)
		case "list":
			resp, err = h.HandleWishlistList(userID, lang)
		default:
			err = errUnknownSubcommand
		}
	case "channel":
		switch subCommandName(data.Options) {
		case "set":
			resp, err = h.HandleChannelSet(i.GuildID, i.ChannelID, lang)
		case "time":
			resp, err = h.HandleChannelTimeMenu(i.GuildID, lang)
		default:
			err = errUnknownSubcommand
		}
	case "language":
		resp, err = h.HandleLanguage(userID, optionString(data.Options, "lang"))
	default:
		log.Printf("interaction: ignoring unknown command %q", data.Name)
		_ = editInteraction(s, i, Response{Content: "Unknown command."})
		return
	}

	if err != nil {
		log.Printf("interaction: /%s error: %v", data.Name, err)
		if rerr := editInteraction(s, i, Response{
			Content: i18n.T(lang, "error.prefix") + err.Error(),
		}); rerr != nil {
			log.Printf("interaction: edit error: %v", rerr)
		}
		return
	}
	resp.Ephemeral = ephemeral
	if rerr := editInteraction(s, i, resp); rerr != nil {
		log.Printf("interaction: edit /%s failed: %v", data.Name, rerr)
	}
}

// qrLoginTimeout bounds how long the bot waits for a Riot Mobile approval.
const qrLoginTimeout = 3 * time.Minute

// shopTimeout bounds /shop's multi-account fetch so the deferred interaction
// edit always fires instead of hanging forever on a stuck upstream request.
const shopTimeout = 45 * time.Second

// watchQRLogin polls until the user approves the QR login, then rewrites the
// ephemeral /auth message with the outcome.
func (h *Handlers) watchQRLogin(s *discordgo.Session, i *discordgo.InteractionCreate, state string, lang i18n.Lang) {
	ctx, cancel := context.WithTimeout(context.Background(), qrLoginTimeout)
	defer cancel()

	display, err := h.Auth.WaitQRLogin(ctx, state)
	if err != nil {
		log.Printf("interaction: qr login state=%s: %v", state, err)
	}
	if rerr := editInteraction(s, i, h.HandleAuthComplete(display, err, lang)); rerr != nil {
		log.Printf("interaction: qr login edit failed: %v", rerr)
	}
}

func commandEphemeral(name string) bool {
	switch name {
	case "shop", "channel":
		return false
	default:
		return true
	}
}

func deferInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, ephemeral bool) error {
	flags := discordgo.MessageFlags(0)
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: flags},
	})
}

func editInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) error {
	var embeds *[]*discordgo.MessageEmbed
	if resp.Embeds != nil {
		embeds = &resp.Embeds
	}
	var components *[]discordgo.MessageComponent
	if resp.Components != nil {
		components = &resp.Components
	}
	content := resp.Content
	// Always reset attachments: uploads in resp.Files are appended by Discord,
	// and the completion edit must drop the QR image left by /auth.
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:     &content,
		Embeds:      embeds,
		Components:  components,
		Files:       resp.Files,
		Attachments: &[]*discordgo.MessageAttachment{},
	})
	return err
}

func (h *Handlers) userLang(discordUserID string) i18n.Lang {
	if h.Lang == nil || discordUserID == "" {
		return i18n.KO
	}
	s, err := h.Lang.GetUserLanguage(discordUserID)
	if err != nil {
		return i18n.KO
	}
	return i18n.Parse(s)
}

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

func optionString(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, o := range opts {
		if o.Name == name {
			return o.StringValue()
		}
	}
	return ""
}

func subCommandName(opts []*discordgo.ApplicationCommandInteractionDataOption) string {
	for _, o := range opts {
		if o.Type == discordgo.ApplicationCommandOptionSubCommand {
			return o.Name
		}
	}
	return ""
}

func subCommandOptions(opts []*discordgo.ApplicationCommandInteractionDataOption) []*discordgo.ApplicationCommandInteractionDataOption {
	for _, o := range opts {
		if o.Type == discordgo.ApplicationCommandOptionSubCommand {
			return o.Options
		}
	}
	return nil
}

func respondInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) error {
	// Kept for tests / direct replies; production path uses defer + edit.
	flags := discordgo.MessageFlags(0)
	if resp.Ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	data := &discordgo.InteractionResponseData{
		Content:    resp.Content,
		Embeds:     resp.Embeds,
		Components: resp.Components,
		Flags:      flags,
	}
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: data,
	})
}
