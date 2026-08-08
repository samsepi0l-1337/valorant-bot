package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/authweb"
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
	ctx, done, ok := h.beginLifecycleWorker(interactionCallbackTimeout)
	if !ok {
		return
	}
	defer done()
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		h.onAppCommandContext(ctx, s, i)
	case discordgo.InteractionMessageComponent:
		h.onComponentContext(ctx, s, i)
	case discordgo.InteractionModalSubmit:
		h.onModalContext(ctx, s, i)
	}
}

func (h *Handlers) onComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	h.onComponentContext(context.Background(), s, i)
}

func (h *Handlers) onComponentContext(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	userID := interactionUserID(i)
	lang := h.cachedUserLang(userID)
	log.Printf("interaction: component %s user=%s", interactionLogCustomID(data.CustomID), userID)

	if data.CustomID == customIDAuthPassword {
		if err := interactionRespond(ctx, s, i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseModal,
			Data: PasswordLoginModal(lang),
		}); err != nil {
			log.Printf("interaction: password modal: %s", discordRESTErrorLog(err))
		}
		return
	}
	if strings.HasPrefix(data.CustomID, customIDAuthMFAOpenPref) {
		mfaState := strings.TrimPrefix(data.CustomID, customIDAuthMFAOpenPref)
		hint, err := h.Auth.ValidatePasswordMFA(mfaState, userID)
		if err != nil {
			h.clearMFAHint(mfaState)
			if errors.Is(err, authweb.ErrMFAOwner) {
				if rerr := respondEphemeral(ctx, s, i, mfaTerminalMessage(lang, err)); rerr != nil {
					log.Printf("interaction: mfa validation response: %s", discordRESTErrorLog(rerr))
				}
				return
			}
			if rerr := deferComponentUpdate(ctx, s, i); rerr != nil {
				log.Printf("interaction: defer stale mfa component: %s", discordRESTErrorLog(rerr))
				return
			}
			lang = h.userLang(userID)
			guard := h.mfaSubmissionGuard(mfaState)
			acquired, guardErr := guard.begin(ctx)
			if guardErr != nil {
				log.Printf("interaction: stale mfa guard: %s", discordRESTErrorLog(guardErr))
				return
			}
			if !acquired {
				return
			}
			terminalApplied := false
			defer func() { guard.finish(terminalApplied) }()
			if rerr := editInteraction(ctx, s, i, Response{
				Content:    mfaTerminalMessage(lang, err),
				Embeds:     []*discordgo.MessageEmbed{},
				Components: []discordgo.MessageComponent{},
			}); rerr != nil {
				log.Printf("interaction: stale mfa component edit: %s", discordRESTErrorLog(rerr))
				return
			}
			terminalApplied = true
			return
		}
		if err := interactionRespond(ctx, s, i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseModal,
			Data: MFALoginModal(mfaState, hint, lang),
		}); err != nil {
			log.Printf("interaction: mfa modal: %s", discordRESTErrorLog(err))
		}
		return
	}
	if data.CustomID == customIDAuthQR {
		if err := deferComponentUpdate(ctx, s, i); err != nil {
			log.Printf("interaction: defer qr component: %s", discordRESTErrorLog(err))
			return
		}
		lang = h.userLang(userID)
		resp, qrState, err := h.HandleAuthQR(ctx, userID, lang)
		if err != nil {
			log.Printf("interaction: qr component error: %v", err)
			if rerr := editInteraction(ctx, s, i, Response{
				Content:    i18n.T(lang, "error.prefix") + err.Error(),
				Embeds:     []*discordgo.MessageEmbed{},
				Components: []discordgo.MessageComponent{},
			}); rerr != nil {
				log.Printf("interaction: qr component error edit failed: %s", discordRESTErrorLog(rerr))
			}
			return
		}
		delivery, rerr := editInteractionWithFilesOutcome(ctx, s, i, resp)
		if rerr != nil {
			log.Printf("interaction: qr component edit failed: %s", discordRESTErrorLog(rerr))
		}
		if qrState != "" {
			if delivery == deliveryRejected {
				h.cancelQRAuth(qrState, userID)
			} else {
				h.startQRLoginWatcher(s, i, qrState, lang)
			}
		}
		return
	}
	if strings.HasPrefix(data.CustomID, customIDAuthCaptchaCancelPref) {
		owner, state, ok := parsePasswordCaptchaCancelCustomID(data.CustomID)
		if !ok {
			if err := respondEphemeral(ctx, s, i, i18n.T(lang, "auth.captcha.expired")); err != nil {
				log.Printf("interaction: malformed captcha cancel response: %s", discordRESTErrorLog(err))
			}
			return
		}
		if owner != userID {
			if err := respondEphemeral(ctx, s, i, i18n.T(lang, "auth.captcha.cancel.denied")); err != nil {
				log.Printf("interaction: captcha cancel owner response: %s", discordRESTErrorLog(err))
			}
			return
		}
		if err := deferComponentUpdate(ctx, s, i); err != nil {
			log.Printf("interaction: defer captcha cancel: %s", discordRESTErrorLog(err))
			return
		}
		lang = h.userLang(userID)
		guard := h.captchaEditGuard(state)
		acquired, guardErr := guard.begin(ctx)
		if guardErr != nil {
			log.Printf("interaction: captcha cancel guard: %s", discordRESTErrorLog(guardErr))
			return
		}
		if !acquired {
			return
		}
		terminalApplied := false
		defer func() { guard.finish(terminalApplied) }()

		canceled, cancelErr := h.cancelPasswordLoginOwned(state, userID)
		if errors.Is(cancelErr, authweb.ErrCaptchaOwner) {
			return
		}
		if cancelErr != nil {
			log.Printf("interaction: captcha cancel: %s", discordRESTErrorLog(cancelErr))
			return
		}
		if !canceled {
			return
		}
		resp := Response{
			Content:    i18n.T(lang, "auth.captcha.cancelled"),
			Embeds:     []*discordgo.MessageEmbed{},
			Components: []discordgo.MessageComponent{},
		}
		delivery, editErr := editInteractionOutcome(ctx, s, i, resp)
		if delivery == deliveryApplied || delivery == deliveryAmbiguous {
			terminalApplied = true
		}
		if editErr != nil {
			log.Printf("interaction: captcha cancel edit: %s", discordRESTErrorLog(editErr))
		}
		return
	}
	if strings.HasPrefix(data.CustomID, customIDAuthCaptchaPref) {
		if err := deferComponentUpdate(ctx, s, i); err != nil {
			log.Printf("interaction: defer captcha component: %s", discordRESTErrorLog(err))
			return
		}
		lang = h.userLang(userID)
		state := strings.TrimPrefix(data.CustomID, customIDAuthCaptchaPref)
		captchaCtx, cancel := context.WithTimeout(ctx, interactionCallbackTimeout)
		defer cancel()
		resp, launched, err := h.handlePasswordCaptchaLaunch(captchaCtx, state, userID, lang)
		if err != nil {
			log.Printf("interaction: captcha component error: %v", err)
			resp = Response{
				Content:    i18n.T(lang, "error.prefix") + err.Error(),
				Embeds:     []*discordgo.MessageEmbed{},
				Components: []discordgo.MessageComponent{},
			}
		}
		terminal := resp.Components != nil && len(resp.Components) == 0
		delivery, rerr := h.editCaptchaInteractionThenOutcome(ctx, s, i, state, resp, terminal, func() {
			if launched {
				h.startPasswordCaptchaWatcher(s, i, state, lang)
			}
		})
		if rerr != nil {
			log.Printf("interaction: captcha component edit failed: %s", discordRESTErrorLog(rerr))
		}
		if launched && (delivery == deliveryRejected || delivery == deliverySuppressed) {
			h.cancelPasswordLogin(state, userID)
		}
		return
	}

	// Resolve ownership and malformed inputs without persistence. All remaining
	// component paths are acknowledged before language/database work.
	switch {
	case strings.HasPrefix(data.CustomID, customIDShopPagePrefix):
		owner, _, noop, ok := parseShopPageCustomID(data.CustomID)
		if !ok {
			_ = updateComponentMessage(ctx, s, i, Response{Content: i18n.T(lang, "error.prefix") + "invalid shop navigation"})
			return
		}
		if noop {
			return
		}
		if owner != userID {
			resp, _ := h.HandleShopNav(owner, 0, userID, lang)
			if rerr := respondEphemeralEmbed(ctx, s, i, resp); rerr != nil {
				log.Printf("interaction: shop owner response: %s", discordRESTErrorLog(rerr))
			}
			return
		}
	case strings.HasPrefix(data.CustomID, customIDWishlistAddPrefix):
		owner := strings.TrimPrefix(data.CustomID, customIDWishlistAddPrefix)
		if owner != userID {
			_ = respondEphemeral(ctx, s, i, i18n.T(lang, "wishlist.pick_denied"))
			return
		}
		if len(data.Values) == 0 {
			_ = updateComponentMessage(ctx, s, i, Response{Content: i18n.T(lang, "error.prefix") + "no selection"})
			return
		}
	case strings.HasPrefix(data.CustomID, customIDWishlistRemovePrefix):
		owner := strings.TrimPrefix(data.CustomID, customIDWishlistRemovePrefix)
		if owner != userID {
			_ = respondEphemeral(ctx, s, i, i18n.T(lang, "wishlist.pick_denied"))
			return
		}
		if len(data.Values) == 0 {
			_ = updateComponentMessage(ctx, s, i, Response{Content: i18n.T(lang, "error.prefix") + "no selection"})
			return
		}
	case strings.HasPrefix(data.CustomID, customIDChannelTimePrefix):
		guildID := strings.TrimPrefix(data.CustomID, customIDChannelTimePrefix)
		if i.GuildID != "" && i.GuildID != guildID {
			_ = respondEphemeral(ctx, s, i, i18n.T(lang, "channel.time_denied"))
			return
		}
		if len(data.Values) == 0 {
			_ = updateComponentMessage(ctx, s, i, Response{Content: i18n.T(lang, "error.prefix") + "no selection"})
			return
		}
	default:
		log.Printf("interaction: ignoring component %q", data.CustomID)
		return
	}
	deferErr := error(nil)
	if strings.HasPrefix(data.CustomID, customIDChannelTimePrefix) {
		// The channel settings menu is public, so preserve its source and send
		// the clicker a separate ephemeral result after the database write.
		deferErr = deferInteraction(ctx, s, i, true)
	} else {
		// Shop navigation is public and wishlist menus are already ephemeral;
		// both should update their originating message.
		deferErr = deferComponentUpdate(ctx, s, i)
	}
	if deferErr != nil {
		log.Printf("interaction: defer component: %s", discordRESTErrorLog(deferErr))
		return
	}
	lang = h.userLang(userID)

	var (
		resp           Response
		err            error
		keepComponents bool
	)
	switch {
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
		_ = editInteraction(ctx, s, i, Response{
			Content:    i18n.T(lang, "error.prefix") + err.Error(),
			Components: []discordgo.MessageComponent{},
		})
		return
	}
	if !keepComponents {
		resp.Components = []discordgo.MessageComponent{}
	}
	if rerr := editInteraction(ctx, s, i, resp); rerr != nil {
		log.Printf("interaction: component update failed: %s", discordRESTErrorLog(rerr))
	}
}

func (h *Handlers) onModal(s *discordgo.Session, i *discordgo.InteractionCreate) {
	h.onModalContext(context.Background(), s, i)
}

func (h *Handlers) onModalContext(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()
	userID := interactionUserID(i)
	log.Printf("interaction: modal %s user=%s", interactionLogCustomID(data.CustomID), userID)

	switch {
	case data.CustomID == customIDAuthPWModal:
		if err := deferInteraction(ctx, s, i, true); err != nil {
			log.Printf("interaction: defer password modal: %s", discordRESTErrorLog(err))
			return
		}
		lang := h.userLang(userID)
		authCtx, cancel := context.WithTimeout(ctx, interactionCallbackTimeout)
		defer cancel()
		resp, passwordState, remote, err := h.handlePasswordLogin(authCtx, userID, modalValue(data, "username"), modalValue(data, "password"), lang)
		if err != nil {
			resp = Response{
				Content:    i18n.T(lang, "error.prefix") + err.Error(),
				Embeds:     []*discordgo.MessageEmbed{},
				Components: []discordgo.MessageComponent{},
			}
		}
		delivery, rerr := editInteractionOutcome(ctx, s, i, resp)
		if rerr != nil {
			log.Printf("interaction: password captcha edit: %s", discordRESTErrorLog(rerr))
		}
		if passwordState != "" {
			switch {
			case delivery == deliveryRejected:
				h.cancelPasswordLogin(passwordState, userID)
			case remote && delivery == deliveryApplied:
				h.startPasswordCaptchaWatcher(s, i, passwordState, lang)
			}
		}
	case strings.HasPrefix(data.CustomID, customIDAuthMFAPref):
		mfaState := strings.TrimPrefix(data.CustomID, customIDAuthMFAPref)
		if _, err := h.Auth.ValidatePasswordMFA(mfaState, userID); err != nil {
			lang := h.cachedUserLang(userID)
			h.clearMFAHint(mfaState)
			if errors.Is(err, authweb.ErrMFAOwner) {
				if rerr := respondEphemeral(ctx, s, i, mfaTerminalMessage(lang, err)); rerr != nil {
					log.Printf("interaction: mfa submit validation response: %s", discordRESTErrorLog(rerr))
				}
				return
			}
			if rerr := deferComponentUpdate(ctx, s, i); rerr != nil {
				log.Printf("interaction: defer stale mfa modal: %s", discordRESTErrorLog(rerr))
				return
			}
			lang = h.userLang(userID)
			guard := h.mfaSubmissionGuard(mfaState)
			acquired, guardErr := guard.begin(ctx)
			if guardErr != nil {
				log.Printf("interaction: stale mfa modal guard: %s", discordRESTErrorLog(guardErr))
				return
			}
			if !acquired {
				return
			}
			terminalApplied := false
			defer func() { guard.finish(terminalApplied) }()
			if rerr := editInteraction(ctx, s, i, Response{
				Content:    mfaTerminalMessage(lang, err),
				Embeds:     []*discordgo.MessageEmbed{},
				Components: []discordgo.MessageComponent{},
			}); rerr != nil {
				log.Printf("interaction: stale mfa modal edit: %s", discordRESTErrorLog(rerr))
				return
			}
			terminalApplied = true
			return
		}
		guard := h.mfaSubmissionGuard(mfaState)
		if err := deferComponentUpdate(ctx, s, i); err != nil {
			log.Printf("interaction: defer mfa modal: %s", discordRESTErrorLog(err))
			return
		}
		lang := h.userLang(userID)
		authCtx, cancel := context.WithTimeout(ctx, interactionCallbackTimeout)
		defer cancel()
		acquired, guardErr := guard.begin(authCtx)
		if guardErr != nil {
			log.Printf("interaction: mfa submit guard: %s", discordRESTErrorLog(guardErr))
			return
		}
		if !acquired {
			return
		}
		terminalApplied := false
		defer func() { guard.finish(terminalApplied) }()
		resp, err := h.HandlePasswordMFA(authCtx, mfaState, userID, modalValue(data, "code"), lang)
		if err != nil {
			resp = Response{
				Content:    i18n.T(lang, "error.prefix") + err.Error(),
				Embeds:     []*discordgo.MessageEmbed{},
				Components: []discordgo.MessageComponent{},
			}
		}
		if rerr := editInteraction(ctx, s, i, resp); rerr != nil {
			log.Printf("interaction: mfa result edit: %s", discordRESTErrorLog(rerr))
			return
		}
		if len(resp.Components) == 0 {
			terminalApplied = true
		}
	default:
		log.Printf("interaction: ignoring modal %q", data.CustomID)
	}
}

func interactionLogCustomID(customID string) string {
	switch {
	case strings.HasPrefix(customID, customIDAuthCaptchaCancelPref):
		return strings.TrimSuffix(customIDAuthCaptchaCancelPref, ":")
	case strings.HasPrefix(customID, customIDAuthCaptchaPref):
		return strings.TrimSuffix(customIDAuthCaptchaPref, ":")
	case strings.HasPrefix(customID, customIDAuthMFAOpenPref):
		return strings.TrimSuffix(customIDAuthMFAOpenPref, ":")
	case strings.HasPrefix(customID, customIDAuthMFAPref):
		return strings.TrimSuffix(customIDAuthMFAPref, ":")
	default:
		return customID
	}
}

func (h *Handlers) mfaSubmissionGuard(state string) *mfaSubmissionGuard {
	h.mfaSubmitMu.Lock()
	if h.mfaSubmitGuards == nil {
		h.mfaSubmitGuards = make(map[string]*mfaSubmissionGuard)
	}
	guard := h.mfaSubmitGuards[state]
	if guard == nil {
		guard = &mfaSubmissionGuard{}
		h.mfaSubmitGuards[state] = guard
		ctx, done, ok := h.beginLifecycleWorker(mfaHintTTL)
		if ok {
			go func() {
				defer done()
				<-ctx.Done()
				h.mfaSubmitMu.Lock()
				if h.mfaSubmitGuards[state] == guard {
					delete(h.mfaSubmitGuards, state)
				}
				h.mfaSubmitMu.Unlock()
			}()
		}
	}
	h.mfaSubmitMu.Unlock()
	return guard
}

func (g *interactionEditGuard) begin(ctx context.Context) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		g.Lock()
		if g.terminal {
			g.Unlock()
			return false, nil
		}
		if !g.busy {
			g.busy = true
			if g.changed == nil {
				g.changed = make(chan struct{})
			}
			g.Unlock()
			return true, nil
		}
		changed := g.changed
		g.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func (g *interactionEditGuard) finish(terminal bool) {
	g.Lock()
	if terminal {
		g.terminal = true
	}
	g.busy = false
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
	g.Unlock()
}

func (g *interactionEditGuard) markTerminal() {
	g.Lock()
	g.terminal = true
	if !g.busy && g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
	g.Unlock()
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

func respondEphemeral(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, content string) error {
	return interactionRespond(ctx, s, i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func respondEphemeralWithComponents(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) error {
	components := resp.Components
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	return interactionRespond(ctx, s, i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:    resp.Content,
			Components: components,
			Flags:      discordgo.MessageFlagsEphemeral,
		},
	})
}

func respondEphemeralEmbed(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) error {
	return interactionRespond(ctx, s, i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: resp.Content,
			Embeds:  resp.Embeds,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func updateComponentMessage(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) error {
	var embeds *[]*discordgo.MessageEmbed
	if resp.Embeds != nil {
		embeds = &resp.Embeds
	}
	components := resp.Components
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	content := resp.Content
	return interactionRespond(ctx, s, i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    content,
			Embeds:     derefEmbeds(embeds),
			Components: components,
		},
	})
}

func deferComponentUpdate(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) error {
	return interactionRespond(ctx, s, i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	})
}

func editInteractionWithFilesOutcome(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) (interactionDeliveryOutcome, error) {
	components := resp.Components
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	content := resp.Content
	embeds := resp.Embeds
	result := interactionEditResult(ctx, s, i.Interaction, &discordgo.WebhookEdit{
		Content:     &content,
		Embeds:      &embeds,
		Components:  &components,
		Files:       resp.Files,
		Attachments: attachmentsForFiles(resp.Files),
	})
	return result.outcome, result.err
}

func attachmentsForFiles(files []*discordgo.File) *[]*discordgo.MessageAttachment {
	attachments := make([]*discordgo.MessageAttachment, 0, len(files))
	for i, file := range files {
		attachments = append(attachments, &discordgo.MessageAttachment{
			ID:       strconv.Itoa(i),
			Filename: file.Name,
		})
	}
	return &attachments
}

func derefEmbeds(p *[]*discordgo.MessageEmbed) []*discordgo.MessageEmbed {
	if p == nil {
		return nil
	}
	return *p
}

func (h *Handlers) onAppCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	h.onAppCommandContext(context.Background(), s, i)
}

func (h *Handlers) onAppCommandContext(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	userID := interactionUserID(i)
	log.Printf("interaction: /%s user=%s guild=%s", data.Name, userID, i.GuildID)

	// ACK within Discord's 3s window before any DB/network work.
	ephemeral := commandEphemeral(data.Name)
	if err := deferInteraction(ctx, s, i, ephemeral); err != nil {
		log.Printf("interaction: defer /%s failed: %s", data.Name, discordRESTErrorLog(err))
		return
	}

	lang := h.userLang(userID)

	var (
		resp Response
		err  error
	)
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
		_ = editInteraction(ctx, s, i, Response{Content: "Unknown command."})
		return
	}

	if err != nil {
		log.Printf("interaction: /%s error: %v", data.Name, err)
		if rerr := editInteraction(ctx, s, i, Response{
			Content: i18n.T(lang, "error.prefix") + err.Error(),
		}); rerr != nil {
			log.Printf("interaction: edit error: %s", discordRESTErrorLog(rerr))
		}
		return
	}
	resp.Ephemeral = ephemeral
	if rerr := editInteraction(ctx, s, i, resp); rerr != nil {
		log.Printf("interaction: edit /%s failed: %s", data.Name, discordRESTErrorLog(rerr))
	}
}

// interactionCallbackTimeout bounds synchronous Discord callback work while
// allowing Shutdown to cancel and join it before dependencies are closed.
const interactionCallbackTimeout = 45 * time.Second

// qrLoginTimeout bounds how long the bot waits for a Riot Mobile approval.
const qrLoginTimeout = 3 * time.Minute

// passwordCaptchaTimeout bounds how long the bot waits for a local or remote Chrome captcha.
const passwordCaptchaTimeout = 10 * time.Minute

// interactionTerminalDeliveryTimeout gives a naturally-timed-out auth wait a
// separate bounded window to replace the Discord controls with its result.
const interactionTerminalDeliveryTimeout = 10 * time.Second

// shopTimeout bounds /shop's multi-account fetch so the deferred interaction
// edit always fires instead of hanging forever on a stuck upstream request.
const shopTimeout = 45 * time.Second

// startQRLoginWatcher polls until the user approves the QR login, then rewrites
// the ephemeral /auth message with the outcome.
func (h *Handlers) startQRLoginWatcher(s *discordgo.Session, i *discordgo.InteractionCreate, state string, lang i18n.Lang) {
	ctx, done, ok := h.beginLifecycleWorker(qrLoginTimeout)
	if !ok {
		return
	}
	go func() {
		defer done()
		h.watchQRLogin(ctx, s, i, state, lang)
	}()
}

func (h *Handlers) watchQRLogin(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, state string, lang i18n.Lang) {
	display, err := h.Auth.WaitQRLogin(ctx, state)
	if err != nil {
		log.Printf("interaction: qr login state=%s: %v", continuationLogValue(state), err)
	}
	deliveryCtx, deliveryDone, ok := h.beginLifecycleWorker(interactionTerminalDeliveryTimeout)
	if !ok {
		return
	}
	defer deliveryDone()
	if rerr := editInteraction(deliveryCtx, s, i, h.HandleAuthComplete(display, err, lang)); rerr != nil {
		log.Printf("interaction: qr login edit failed: %s", discordRESTErrorLog(rerr))
	}
}

// startPasswordCaptchaWatcher starts at most one watcher for a live CAPTCHA state.
// Reopening Chrome uses the existing watcher rather than creating another waiter.
func (h *Handlers) startPasswordCaptchaWatcher(s *discordgo.Session, i *discordgo.InteractionCreate, state string, lang i18n.Lang) {
	h.captchaWatchMu.Lock()
	if h.captchaWatches == nil {
		h.captchaWatches = make(map[string]struct{})
	}
	if _, watching := h.captchaWatches[state]; watching {
		h.captchaWatchMu.Unlock()
		return
	}
	ctx, done, ok := h.beginLifecycleWorker(passwordCaptchaTimeout)
	if !ok {
		h.captchaWatchMu.Unlock()
		return
	}
	h.captchaWatches[state] = struct{}{}
	h.captchaWatchMu.Unlock()

	go func() {
		defer func() {
			h.captchaWatchMu.Lock()
			delete(h.captchaWatches, state)
			h.captchaWatchMu.Unlock()
			done()
		}()
		h.watchPasswordCaptcha(ctx, s, i, state, lang)
	}()
}

// watchPasswordCaptcha waits for the browser captcha page, then shows MFA step 2 or success.
func (h *Handlers) watchPasswordCaptcha(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, state string, lang i18n.Lang) {
	display, mfaState, mfaHint, err := h.Auth.WaitPasswordLogin(ctx, state)
	if err != nil {
		log.Printf("interaction: password captcha state=%s: %v", continuationLogValue(state), err)
	}
	resp := h.HandlePasswordCaptchaComplete(display, mfaState, mfaHint, err, lang)
	deliveryCtx, deliveryDone, ok := h.beginLifecycleWorker(interactionTerminalDeliveryTimeout)
	if !ok {
		if mfaState != "" {
			h.cancelPasswordMFA(mfaState, interactionUserID(i))
		}
		return
	}
	defer deliveryDone()
	delivery, rerr := h.editCaptchaInteractionOutcome(deliveryCtx, s, i, state, resp, true)
	if rerr != nil {
		log.Printf("interaction: password captcha edit failed: %s", discordRESTErrorLog(rerr))
	}
	if mfaState != "" && (delivery == deliveryRejected || delivery == deliverySuppressed) {
		h.cancelPasswordMFA(mfaState, interactionUserID(i))
	}
}

func (h *Handlers) editCaptchaInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, state string, resp Response, terminal bool) error {
	return h.editCaptchaInteractionContext(context.Background(), s, i, state, resp, terminal)
}

func (h *Handlers) editCaptchaInteractionContext(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, state string, resp Response, terminal bool) error {
	return h.editCaptchaInteractionThen(ctx, s, i, state, resp, terminal, nil)
}

func (h *Handlers) editCaptchaInteractionOutcome(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, state string, resp Response, terminal bool) (interactionDeliveryOutcome, error) {
	return h.editCaptchaInteractionThenOutcome(ctx, s, i, state, resp, terminal, nil)
}

// editCaptchaInteractionThen serializes a CAPTCHA edit and its dependent
// action under the same per-state guard. This prevents a terminal completion
// from landing between a reopen status edit and watcher enrollment.
func (h *Handlers) editCaptchaInteractionThen(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, state string, resp Response, terminal bool, afterApplied func()) error {
	_, err := h.editCaptchaInteractionThenOutcome(ctx, s, i, state, resp, terminal, afterApplied)
	return err
}

func (h *Handlers) editCaptchaInteractionThenOutcome(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, state string, resp Response, terminal bool, afterApplied func()) (interactionDeliveryOutcome, error) {
	guard := h.captchaEditGuard(state)
	acquired, err := guard.begin(ctx)
	if err != nil {
		return deliveryRejected, err
	}
	if !acquired {
		return deliverySuppressed, nil
	}
	terminalApplied := false
	defer func() { guard.finish(terminalApplied) }()
	delivery, err := editInteractionOutcome(ctx, s, i, resp)
	if terminal && (delivery == deliveryApplied || delivery == deliveryAmbiguous) {
		// An ambiguous terminal edit may already be visible. Preserve the same
		// monotonic boundary as a confirmed edit so a reopen cannot overwrite it.
		terminalApplied = true
	}
	if afterApplied != nil && (delivery == deliveryApplied || delivery == deliveryAmbiguous) {
		afterApplied()
	}
	return delivery, err
}

func continuationLogValue(string) string { return "<redacted>" }

func discordRESTErrorLog(err error) string {
	if err == nil {
		return "discord REST error type=<nil>"
	}

	var rateLimitErr *discordgo.RateLimitError
	if errors.As(err, &rateLimitErr) {
		result := "discord REST error type=*discordgo.RateLimitError status=429"
		if rateLimitErr != nil && rateLimitErr.RateLimit != nil && rateLimitErr.TooManyRequests != nil {
			result += fmt.Sprintf(" retry_after=%s", rateLimitErr.RetryAfter)
		}
		return result
	}

	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) {
		result := "discord REST error type=*discordgo.RESTError"
		if restErr != nil && restErr.Response != nil {
			result += fmt.Sprintf(" status=%d", restErr.Response.StatusCode)
		}
		if restErr != nil && restErr.Message != nil {
			result += fmt.Sprintf(" code=%d", restErr.Message.Code)
		}
		return result
	}

	var transportErr *url.Error
	if errors.As(err, &transportErr) {
		return "discord REST error type=*url.Error"
	}

	return fmt.Sprintf("discord REST error type=%T", err)
}

func commandEphemeral(name string) bool {
	switch name {
	case "shop", "channel":
		return false
	default:
		return true
	}
}

func deferInteraction(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, ephemeral bool) error {
	flags := discordgo.MessageFlags(0)
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	return interactionRespond(ctx, s, i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: flags},
	})
}

func editInteraction(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) error {
	_, err := editInteractionOutcome(ctx, s, i, resp)
	return err
}

func editInteractionOutcome(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, resp Response) (interactionDeliveryOutcome, error) {
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
	result := interactionEditResult(ctx, s, i.Interaction, &discordgo.WebhookEdit{
		Content:     &content,
		Embeds:      embeds,
		Components:  components,
		Files:       resp.Files,
		Attachments: &[]*discordgo.MessageAttachment{},
	})
	return result.outcome, result.err
}

func (h *Handlers) userLang(discordUserID string) i18n.Lang {
	if h.Lang == nil || discordUserID == "" {
		return h.cachedUserLang(discordUserID)
	}
	s, err := h.Lang.GetUserLanguage(discordUserID)
	if err != nil {
		return h.cachedUserLang(discordUserID)
	}
	lang := i18n.Parse(s)
	h.cacheUserLang(discordUserID, lang)
	return lang
}

// cachedUserLang is safe on Discord's modal-opening deadline: it never reads
// the database and falls back to Korean until a safely acknowledged lookup has
// populated the cache.
func (h *Handlers) cachedUserLang(discordUserID string) i18n.Lang {
	h.langMu.RLock()
	lang := h.langCache[discordUserID]
	h.langMu.RUnlock()
	if lang == "" {
		return i18n.KO
	}
	return lang
}

func (h *Handlers) cacheUserLang(discordUserID string, lang i18n.Lang) {
	if discordUserID == "" {
		return
	}
	h.langMu.Lock()
	if h.langCache == nil {
		h.langCache = make(map[string]i18n.Lang)
	}
	h.langCache[discordUserID] = lang
	h.langMu.Unlock()
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
	return interactionRespond(context.Background(), s, i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: data,
	})
}
