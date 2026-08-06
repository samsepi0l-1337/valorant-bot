package bot

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

const (
	customIDWishlistAddPrefix    = "wishlist:add:"
	customIDWishlistRemovePrefix = "wishlist:remove:"
	customIDChannelTimePrefix    = "channel:time:"
)

// HandleWishlistAdd searches skins and shows a select menu to pick the exact name.
func (h *Handlers) HandleWishlistAdd(discordUserID, query string, lang i18n.Lang) (Response, error) {
	if h.Wishlist == nil || h.Skins == nil {
		return Response{}, fmt.Errorf("wishlist not configured")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return Response{}, fmt.Errorf("query is required")
	}
	if err := h.Skins.EnsureLoaded(context.Background(), string(lang)); err != nil {
		return Response{}, err
	}
	matches := h.Skins.SearchByName(query, string(lang))
	if len(matches) == 0 {
		return Response{
			Ephemeral: true,
			Content:   i18n.T(lang, "wishlist.not_found"),
		}, nil
	}
	return Response{
		Ephemeral:  true,
		Content:    fmt.Sprintf(i18n.T(lang, "wishlist.pick_add"), len(matches), query),
		Components: []discordgo.MessageComponent{wishlistSelectRow(customIDWishlistAddPrefix+discordUserID, i18n.T(lang, "wishlist.pick_placeholder"), matches)},
	}, nil
}

// HandleWishlistRemove removes a wishlist entry, or shows a select menu when ambiguous.
func (h *Handlers) HandleWishlistRemove(discordUserID, identifier string, lang i18n.Lang) (Response, error) {
	if h.Wishlist == nil {
		return Response{}, fmt.Errorf("wishlist not configured")
	}
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return Response{}, fmt.Errorf("identifier is required")
	}
	items, err := h.Wishlist.ListWishlists(discordUserID)
	if err != nil {
		return Response{}, err
	}
	if len(items) == 0 {
		return Response{
			Ephemeral: true,
			Content:   i18n.T(lang, "wishlist.empty"),
		}, nil
	}
	var matches []store.WishlistItem
	for _, w := range items {
		if w.SkinUUID == identifier {
			matches = []store.WishlistItem{w}
			break
		}
	}
	if len(matches) == 0 {
		lower := strings.ToLower(identifier)
		for _, w := range items {
			if strings.Contains(strings.ToLower(w.SkinName), lower) || strings.Contains(strings.ToLower(w.SkinUUID), lower) {
				matches = append(matches, w)
			}
		}
	}
	switch len(matches) {
	case 0:
		return Response{
			Ephemeral: true,
			Content:   i18n.T(lang, "wishlist.remove_not_found"),
		}, nil
	case 1:
		return h.removeWishlistItem(discordUserID, matches[0], lang)
	default:
		opts := make([]skins.Skin, 0, len(matches))
		for _, w := range matches {
			name := w.SkinName
			if name == "" {
				name = w.SkinUUID
			}
			opts = append(opts, skins.Skin{UUID: w.SkinUUID, DisplayName: name})
		}
		if len(opts) > 25 {
			opts = opts[:25]
		}
		return Response{
			Ephemeral:  true,
			Content:    i18n.T(lang, "wishlist.pick_remove"),
			Components: []discordgo.MessageComponent{wishlistSelectRow(customIDWishlistRemovePrefix+discordUserID, i18n.T(lang, "wishlist.pick_placeholder"), opts)},
		}, nil
	}
}

func (h *Handlers) removeWishlistItem(discordUserID string, w store.WishlistItem, lang i18n.Lang) (Response, error) {
	if err := h.Wishlist.RemoveWishlist(discordUserID, w.SkinUUID); err != nil {
		return Response{}, err
	}
	name := w.SkinName
	if name == "" {
		name = w.SkinUUID
	}
	return Response{
		Ephemeral: true,
		Content:   fmt.Sprintf(i18n.T(lang, "wishlist.removed"), name),
	}, nil
}

// HandleWishlistSelectAdd completes add after the user picks from the select menu.
func (h *Handlers) HandleWishlistSelectAdd(discordUserID, skinUUID string, lang i18n.Lang) (Response, error) {
	if h.Wishlist == nil || h.Skins == nil {
		return Response{}, fmt.Errorf("wishlist not configured")
	}
	if err := h.Skins.EnsureLoaded(context.Background(), string(lang)); err != nil {
		return Response{}, err
	}
	skin, ok := h.Skins.Get(skinUUID, string(lang))
	name := skinUUID
	if ok && skin.DisplayName != "" {
		name = skin.DisplayName
		skinUUID = skin.UUID
	}
	if err := h.Wishlist.AddWishlist(discordUserID, skinUUID, name); err != nil {
		return Response{}, err
	}
	return Response{
		Ephemeral: true,
		Content:   fmt.Sprintf(i18n.T(lang, "wishlist.added"), name),
	}, nil
}

// HandleWishlistSelectRemove completes remove after the user picks from the select menu.
func (h *Handlers) HandleWishlistSelectRemove(discordUserID, skinUUID string, lang i18n.Lang) (Response, error) {
	if h.Wishlist == nil {
		return Response{}, fmt.Errorf("wishlist not configured")
	}
	items, err := h.Wishlist.ListWishlists(discordUserID)
	if err != nil {
		return Response{}, err
	}
	for _, w := range items {
		if w.SkinUUID == skinUUID {
			return h.removeWishlistItem(discordUserID, w, lang)
		}
	}
	return Response{Ephemeral: true, Content: i18n.T(lang, "wishlist.remove_not_found")}, nil
}

func wishlistSelectRow(customID, placeholder string, matches []skins.Skin) discordgo.ActionsRow {
	if len(matches) > 25 {
		matches = matches[:25]
	}
	options := make([]discordgo.SelectMenuOption, 0, len(matches))
	seen := map[string]struct{}{}
	for _, s := range matches {
		if _, ok := seen[s.UUID]; ok {
			continue
		}
		seen[s.UUID] = struct{}{}
		label := s.DisplayName
		if label == "" {
			label = s.UUID
		}
		label = truncateRunes(label, 100)
		options = append(options, discordgo.SelectMenuOption{
			Label: label,
			Value: s.UUID,
		})
		if len(options) >= 25 {
			break
		}
	}
	return discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{
				CustomID:    customID,
				Placeholder: truncateRunes(placeholder, 150),
				MinValues:   ptrInt(1),
				MaxValues:   1,
				Options:     options,
			},
		},
	}
}

func ptrInt(v int) *int { return &v }

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// HandleWishlistList lists wishlist items for the user.
func (h *Handlers) HandleWishlistList(discordUserID string, lang i18n.Lang) (Response, error) {
	if h.Wishlist == nil {
		return Response{}, fmt.Errorf("wishlist not configured")
	}
	items, err := h.Wishlist.ListWishlists(discordUserID)
	if err != nil {
		return Response{}, err
	}
	if len(items) == 0 {
		return Response{
			Ephemeral: true,
			Content:   i18n.T(lang, "wishlist.empty"),
		}, nil
	}
	var b strings.Builder
	b.WriteString(i18n.T(lang, "wishlist.header") + "\n")
	for _, w := range items {
		name := w.SkinName
		if name == "" {
			name = w.SkinUUID
		}
		fmt.Fprintf(&b, "• %s\n", name)
	}
	return Response{Ephemeral: true, Content: strings.TrimSpace(b.String())}, nil
}

// HandleChannelSet saves the current channel as the guild daily-notification channel.
func (h *Handlers) HandleChannelSet(guildID, channelID string, lang i18n.Lang) (Response, error) {
	if h.Guilds == nil {
		return Response{}, fmt.Errorf("guild settings not configured")
	}
	if guildID == "" || channelID == "" {
		return Response{}, fmt.Errorf("guild and channel are required")
	}
	hour := store.DefaultDailyHourKST
	if existing, ok, err := h.Guilds.GetGuildSettings(guildID); err == nil && ok {
		hour = existing.DailyHour
	}
	if err := h.Guilds.UpsertGuildSettings(store.GuildSettings{
		GuildID:        guildID,
		DailyChannelID: channelID,
		Enabled:        true,
		DailyHour:      hour,
	}); err != nil {
		return Response{}, err
	}
	return Response{
		Content: fmt.Sprintf(i18n.T(lang, "channel.set"), channelID, hour),
	}, nil
}

// HandleChannelTimeMenu shows a select menu to pick the daily post hour (KST).
func (h *Handlers) HandleChannelTimeMenu(guildID string, lang i18n.Lang) (Response, error) {
	if h.Guilds == nil {
		return Response{}, fmt.Errorf("guild settings not configured")
	}
	if guildID == "" {
		return Response{}, fmt.Errorf("guild only")
	}
	current := store.DefaultDailyHourKST
	if existing, ok, err := h.Guilds.GetGuildSettings(guildID); err == nil && ok {
		current = existing.DailyHour
	}
	return Response{
		Ephemeral:  true,
		Content:    fmt.Sprintf(i18n.T(lang, "channel.time_prompt"), current),
		Components: []discordgo.MessageComponent{dailyHourSelectRow(customIDChannelTimePrefix+guildID, lang)},
	}, nil
}

// HandleChannelTimeSelect saves the chosen KST hour for daily posts.
func (h *Handlers) HandleChannelTimeSelect(guildID, hourStr string, lang i18n.Lang) (Response, error) {
	if h.Guilds == nil {
		return Response{}, fmt.Errorf("guild settings not configured")
	}
	var hour int
	if _, err := fmt.Sscanf(hourStr, "%d", &hour); err != nil || hour < 0 || hour > 23 {
		return Response{Ephemeral: true, Content: i18n.T(lang, "channel.time_invalid")}, nil
	}
	channelID := ""
	enabled := true
	if existing, ok, err := h.Guilds.GetGuildSettings(guildID); err == nil && ok {
		channelID = existing.DailyChannelID
		enabled = existing.Enabled
	}
	if err := h.Guilds.UpsertGuildSettings(store.GuildSettings{
		GuildID:        guildID,
		DailyChannelID: channelID,
		Enabled:        enabled,
		DailyHour:      hour,
	}); err != nil {
		return Response{}, err
	}
	msg := fmt.Sprintf(i18n.T(lang, "channel.time_set"), hour)
	if channelID == "" {
		msg += "\n" + i18n.T(lang, "channel.time_need_set")
	}
	return Response{Ephemeral: true, Content: msg}, nil
}

func dailyHourSelectRow(customID string, lang i18n.Lang) discordgo.ActionsRow {
	options := make([]discordgo.SelectMenuOption, 0, 24)
	for h := 0; h < 24; h++ {
		label := fmt.Sprintf("%02d:00 KST", h)
		desc := ""
		if h == store.DefaultDailyHourKST {
			if lang == i18n.EN {
				label = fmt.Sprintf("%02d:00 KST (store reset)", h)
				desc = "Recommended"
			} else {
				label = fmt.Sprintf("%02d:00 KST (상점 리셋)", h)
				desc = "권장"
			}
		}
		options = append(options, discordgo.SelectMenuOption{
			Label:       truncateRunes(label, 100),
			Value:       fmt.Sprintf("%d", h),
			Description: desc,
		})
	}
	placeholder := i18n.T(lang, "channel.time_placeholder")
	return discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{
				CustomID:    customID,
				Placeholder: truncateRunes(placeholder, 150),
				MinValues:   ptrInt(1),
				MaxValues:   1,
				Options:     options,
			},
		},
	}
}
