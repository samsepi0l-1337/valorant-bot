package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/bot"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
	"github.com/dosfsociety/valorant-bot/internal/store"
	"github.com/robfig/cron/v3"
)

// GuildStore lists enabled guild daily-post settings.
type GuildStore interface {
	ListEnabledGuildSettings() ([]store.GuildSettings, error)
}

// AccountLister lists all linked Riot accounts.
type AccountLister interface {
	ListAllRiotAccounts() ([]store.Account, error)
}

// WishlistLister lists all wishlist items.
type WishlistLister interface {
	ListAllWishlists() ([]store.WishlistItem, error)
}

// ShopSource fetches shop views for a Discord user.
type ShopSource interface {
	ShopsForUser(ctx context.Context, discordUserID, language string) ([]bot.AccountShop, error)
}

// ChannelPoster posts messages to a guild text channel.
type ChannelPoster interface {
	PostChannel(ctx context.Context, channelID, content string, embeds []*discordgo.MessageEmbed) error
}

// DMPoster sends a direct message to a Discord user.
type DMPoster interface {
	SendDM(ctx context.Context, discordUserID, content string, embeds []*discordgo.MessageEmbed) error
}

// Scheduler runs the daily shop post + wishlist DM pass.
type Scheduler struct {
	Guilds    GuildStore
	Accounts  AccountLister
	Wishlists WishlistLister
	Shops     ShopSource
	Channels  ChannelPoster
	DMs       DMPoster

	mu               sync.Mutex
	lastWishlistDate string // YYYY-MM-DD in Asia/Seoul
}

// Seoul is the timezone used for daily schedule hours.
func Seoul() *time.Location {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		return time.FixedZone("KST", 9*60*60)
	}
	return loc
}

// RunOnce performs a single daily shop / wishlist pass for all enabled guilds.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	return s.RunForHour(ctx, -1)
}

// RunForHour posts shops to guilds whose DailyHour matches hourKST.
// hourKST < 0 means all enabled guilds (manual / test RunOnce).
func (s *Scheduler) RunForHour(ctx context.Context, hourKST int) error {
	guilds, err := s.Guilds.ListEnabledGuildSettings()
	if err != nil {
		return fmt.Errorf("list guild settings: %w", err)
	}

	var target []store.GuildSettings
	for _, gs := range guilds {
		if gs.DailyChannelID == "" {
			continue
		}
		if hourKST >= 0 && gs.DailyHour != hourKST {
			continue
		}
		target = append(target, gs)
	}
	if len(target) == 0 && hourKST >= 0 {
		return nil
	}

	accounts, err := s.Accounts.ListAllRiotAccounts()
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}

	shopsByUser := map[string][]bot.AccountShop{}
	seenUsers := map[string]struct{}{}
	var allShops []bot.AccountShop
	for _, acc := range accounts {
		if _, ok := seenUsers[acc.DiscordUserID]; ok {
			continue
		}
		seenUsers[acc.DiscordUserID] = struct{}{}
		shops, err := s.Shops.ShopsForUser(ctx, acc.DiscordUserID, string(i18n.KO))
		if err != nil {
			return fmt.Errorf("shops for %s: %w", acc.DiscordUserID, err)
		}
		shopsByUser[acc.DiscordUserID] = shops
		allShops = append(allShops, shops...)
	}

	embeds := bot.BuildShopEmbeds(allShops, i18n.KO)
	postGuilds := target
	if hourKST < 0 {
		postGuilds = nil
		for _, gs := range guilds {
			if gs.DailyChannelID != "" {
				postGuilds = append(postGuilds, gs)
			}
		}
	}
	for _, gs := range postGuilds {
		if err := s.Channels.PostChannel(ctx, gs.DailyChannelID, "Daily Valorant shop", embeds); err != nil {
			return fmt.Errorf("post channel %s: %w", gs.DailyChannelID, err)
		}
	}

	// Wishlist DMs once per KST calendar day (tied to the first hourly tick that runs).
	today := time.Now().In(Seoul()).Format("2006-01-02")
	s.mu.Lock()
	already := s.lastWishlistDate == today
	if !already {
		s.lastWishlistDate = today
	}
	s.mu.Unlock()
	if already && hourKST >= 0 {
		return nil
	}

	wishlists, err := s.Wishlists.ListAllWishlists()
	if err != nil {
		return fmt.Errorf("list wishlists: %w", err)
	}

	dmmed := map[string]struct{}{}
	for _, w := range wishlists {
		key := w.DiscordUserID + "\x00" + w.SkinUUID
		if _, ok := dmmed[key]; ok {
			continue
		}
		if !shopContainsSkin(shopsByUser[w.DiscordUserID], w.SkinUUID) {
			continue
		}
		name := w.SkinName
		if name == "" {
			name = w.SkinUUID
		}
		msg := fmt.Sprintf("Wishlist hit: %s is in your shop today!", name)
		if err := s.DMs.SendDM(ctx, w.DiscordUserID, msg, nil); err != nil {
			return fmt.Errorf("dm %s: %w", w.DiscordUserID, err)
		}
		dmmed[key] = struct{}{}
	}

	return nil
}

func shopContainsSkin(shops []bot.AccountShop, skinUUID string) bool {
	for _, shop := range shops {
		for _, o := range shop.Offers {
			if o.SkinUUID == skinUUID {
				return true
			}
		}
	}
	return false
}

// Start ticks every minute in Asia/Seoul and runs guild posts at each guild's DailyHour.
// cronExpr is kept for backward compatibility but ignored for guild schedules
// (except empty: still starts the Seoul ticker).
func (s *Scheduler) Start(ctx context.Context, cronExpr string) error {
	_ = cronExpr
	loc := Seoul()
	c := cron.New(cron.WithLocation(loc))
	_, err := c.AddFunc("* * * * *", func() {
		now := time.Now().In(loc)
		if now.Minute() != 0 {
			return
		}
		_ = s.RunForHour(ctx, now.Hour())
	})
	if err != nil {
		return fmt.Errorf("cron: %w", err)
	}
	c.Start()
	defer c.Stop()

	<-ctx.Done()
	return ctx.Err()
}
