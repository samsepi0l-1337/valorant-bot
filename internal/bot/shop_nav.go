package bot

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
)

const (
	customIDShopPagePrefix = "shop:page:"
	shopCacheTTL           = 10 * time.Minute
)

type shopCacheEntry struct {
	shops     []AccountShop
	lang      i18n.Lang
	expiresAt time.Time
}

type shopPageCache struct {
	mu     sync.Mutex
	byUser map[string]shopCacheEntry
}

func newShopPageCache() *shopPageCache {
	return &shopPageCache{byUser: make(map[string]shopCacheEntry)}
}

func (c *shopPageCache) put(userID string, shops []AccountShop, lang i18n.Lang) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byUser[userID] = shopCacheEntry{
		shops:     shops,
		lang:      lang,
		expiresAt: time.Now().Add(shopCacheTTL),
	}
}

func (c *shopPageCache) get(userID string) (shopCacheEntry, bool) {
	if c == nil {
		return shopCacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byUser[userID]
	if !ok || time.Now().After(e.expiresAt) {
		delete(c.byUser, userID)
		return shopCacheEntry{}, false
	}
	return e, true
}

func (h *Handlers) ensureShopCache() *shopPageCache {
	if h.shopCache == nil {
		h.shopCache = newShopPageCache()
	}
	return h.shopCache
}

// shopPageResponse builds the Discord message for one account page.
func shopPageResponse(ownerID string, shops []AccountShop, page int, lang i18n.Lang) Response {
	if page < 0 {
		page = 0
	}
	if page >= len(shops) {
		page = len(shops) - 1
	}
	if page < 0 {
		return Response{Content: i18n.T(lang, "shop.empty")}
	}

	total := len(shops)
	account := FormatShopAccountLabel(shops[page], lang)
	content := fmt.Sprintf("**%s**", account)
	if total > 1 {
		content = fmt.Sprintf("%s\n%s", content, fmt.Sprintf(i18n.T(lang, "shop.page"), page+1, total))
	}
	resp := Response{
		Content: content,
		// One account per page; up to 4 skin embeds so each item has an image.
		Embeds: BuildAccountPageEmbeds(shops[page], lang),
	}
	if total > 1 {
		resp.Components = []discordgo.MessageComponent{shopNavRow(ownerID, page, total, lang)}
	}
	return resp
}

func shopNavRow(ownerID string, page, total int, lang i18n.Lang) discordgo.ActionsRow {
	prev := page - 1
	if prev < 0 {
		prev = 0
	}
	next := page + 1
	if next >= total {
		next = total - 1
	}
	return discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    i18n.T(lang, "shop.prev"),
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("%s%s:%d", customIDShopPagePrefix, ownerID, prev),
				Disabled: page <= 0,
			},
			discordgo.Button{
				Label:    fmt.Sprintf("%d / %d", page+1, total),
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("%s%s:noop", customIDShopPagePrefix, ownerID),
				Disabled: true,
			},
			discordgo.Button{
				Label:    i18n.T(lang, "shop.next"),
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("%s%s:%d", customIDShopPagePrefix, ownerID, next),
				Disabled: page >= total-1,
			},
		},
	}
}

// parseShopPageCustomID parses shop:page:{owner}:{page|noop}.
func parseShopPageCustomID(customID string) (ownerID string, page int, noop bool, ok bool) {
	if !strings.HasPrefix(customID, customIDShopPagePrefix) {
		return "", 0, false, false
	}
	rest := strings.TrimPrefix(customID, customIDShopPagePrefix)
	// owner is a Discord snowflake (digits); page is trailing :N or :noop
	idx := strings.LastIndex(rest, ":")
	if idx <= 0 {
		return "", 0, false, false
	}
	ownerID = rest[:idx]
	tail := rest[idx+1:]
	if tail == "noop" {
		return ownerID, 0, true, true
	}
	page, err := strconv.Atoi(tail)
	if err != nil || page < 0 {
		return "", 0, false, false
	}
	return ownerID, page, false, true
}

// HandleShopNav serves a cached shop page for prev/next buttons.
// Denial is an ephemeral embed (only the clicker sees it); it must not
// rewrite the shared /shop message.
func (h *Handlers) HandleShopNav(ownerID string, page int, clickerID string, lang i18n.Lang) (Response, error) {
	if ownerID != clickerID {
		return Response{
			Ephemeral: true,
			Embeds: []*discordgo.MessageEmbed{{
				Description: i18n.T(lang, "shop.nav_denied"),
				Color:       0xFD4553,
			}},
		}, nil
	}
	entry, ok := h.ensureShopCache().get(ownerID)
	if !ok {
		return Response{}, fmt.Errorf("%s", i18n.T(lang, "shop.nav_expired"))
	}
	useLang := entry.lang
	if useLang == "" {
		useLang = lang
	}
	return shopPageResponse(ownerID, entry.shops, page, useLang), nil
}
