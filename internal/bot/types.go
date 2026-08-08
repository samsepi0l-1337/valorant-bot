package bot

import (
	"context"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

// AuthStarter drives Riot account linking (authweb.Server in production).
type AuthStarter interface {
	BeginQRAuth(ctx context.Context, discordUserID string) (loginURL string, state string, err error)
	WaitQRLogin(ctx context.Context, state string) (displayName string, err error)
	// BeginPasswordLogin prepares an owner-bound password flow. A non-empty URL
	// selects the bot-host remote relay; an empty URL selects the local/disabled
	// button flow.
	BeginPasswordLogin(ctx context.Context, discordUserID, username, password string) (captchaURL, state string, err error)
	LaunchPasswordCaptcha(ctx context.Context, state, discordUserID string) error
	WaitPasswordLogin(ctx context.Context, state string) (displayName, mfaState, mfaHint string, err error)
	ValidatePasswordMFA(mfaState, discordUserID string) (hint string, err error)
	CancelPasswordMFA(mfaState, discordUserID string) error
	CompletePasswordMFA(ctx context.Context, mfaState, discordUserID, code string) (displayName string, err error)
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
	// Color is the Discord embed left-border color from skin rarity.
	Color int
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

	langMu    sync.RWMutex
	langCache map[string]i18n.Lang

	lifecycleMu       sync.Mutex
	lifecycleCtx      context.Context
	lifecycleCancel   context.CancelFunc
	lifecycleClosed   bool
	lifecycleWG       sync.WaitGroup
	lifecycleDone     chan struct{}
	lifecycleWaitOnce sync.Once

	shopCache *shopPageCache

	captchaWatchMu sync.Mutex
	captchaWatches map[string]struct{}
	captchaEditMu  sync.Mutex
	captchaEdits   map[string]*captchaEditGuard

	mfaHintMu sync.Mutex
	mfaHints  map[string]string // mfaState → email hint or "authenticator"

	mfaSubmitMu     sync.Mutex
	mfaSubmitGuards map[string]*mfaSubmissionGuard
}

type interactionEditGuard struct {
	sync.Mutex
	terminal bool
	busy     bool
	changed  chan struct{}
}

type captchaEditGuard struct{ interactionEditGuard }

type mfaSubmissionGuard struct{ interactionEditGuard }

// Response is a Discord reply shape tests can assert without the gateway.
type Response struct {
	Ephemeral  bool
	Content    string
	Embeds     []*discordgo.MessageEmbed
	Components []discordgo.MessageComponent
	Files      []*discordgo.File
}
