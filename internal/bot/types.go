package bot

import (
	"context"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

// AuthStarter is provided by authweb later; mock in tests.
type AuthStarter interface {
	BeginAuth(discordUserID string) (loginURL string, state string, err error)
}

// AccountStore lists and deletes linked Riot accounts for a Discord user.
type AccountStore interface {
	ListRiotAccountsByDiscord(discordUserID string) ([]store.Account, error)
	DeleteRiotAccount(discordUserID, puuid string) error
}

// ShopService fetches shop views for all accounts of a Discord user.
type ShopService interface {
	ShopsForUser(ctx context.Context, discordUserID, language string) ([]AccountShop, error)
}

// OfferView is a single skin offer for display.
type OfferView struct {
	DisplayName string
	IconURL     string
	CostVP      int
	SkinUUID    string
}

// AccountShop is the shop state for one linked account.
type AccountShop struct {
	GameName string
	TagLine  string
	Region   string
	PUUID    string
	Offers   []OfferView
	Err      string
}

// SkinSearcher finds skins by display name (skins.Cache in production).
type SkinSearcher interface {
	SearchByName(query, language string) []skins.Skin
	EnsureLoaded(ctx context.Context, language string) error
	Get(uuid, language string) (skins.Skin, bool)
}

// WishlistStore manages per-user skin wishlists.
type WishlistStore interface {
	AddWishlist(discordUserID, skinUUID, skinName string) error
	RemoveWishlist(discordUserID, skinUUID string) error
	ListWishlists(discordUserID string) ([]store.WishlistItem, error)
}

// GuildStore persists per-guild notification settings.
type GuildStore interface {
	UpsertGuildSettings(gs store.GuildSettings) error
	GetGuildSettings(guildID string) (store.GuildSettings, bool, error)
}

// LanguageStore persists per-user UI language.
type LanguageStore interface {
	GetUserLanguage(discordUserID string) (string, error)
	SetUserLanguage(discordUserID, language string) error
}

// Handlers holds dependencies for slash-command logic.
type Handlers struct {
	Auth     AuthStarter
	Accounts AccountStore
	Shops    ShopService
	Skins    SkinSearcher
	Wishlist WishlistStore
	Guilds   GuildStore
	Lang     LanguageStore
}

// Response is a Discord reply shape tests can assert without the gateway.
type Response struct {
	Ephemeral  bool
	Content    string
	Embeds     []*discordgo.MessageEmbed
	Components []discordgo.MessageComponent
}
