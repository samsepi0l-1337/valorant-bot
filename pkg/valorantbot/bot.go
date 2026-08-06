package valorantbot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/authweb"
	"github.com/dosfsociety/valorant-bot/internal/bot"
	"github.com/dosfsociety/valorant-bot/internal/crypto"
	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/scheduler"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

// Config holds settings needed to construct the bot.
// Fields mirror internal/config for public package consumers.
type Config struct {
	DiscordToken   string
	DiscordAppID   string
	DiscordGuildID string
	BotSecret      string
	AuthPort       int
	AuthBaseURL    string
	DatabasePath   string
	StoreResetCron string
}

// Bot is the Valorant Discord store bot.
type Bot struct {
	cfg Config
}

// New constructs a Bot from Config.
func New(cfg Config) (*Bot, error) {
	if cfg.DiscordToken == "" {
		return nil, errors.New("DiscordToken is required")
	}
	if cfg.DiscordAppID == "" {
		return nil, errors.New("DiscordAppID is required")
	}
	if cfg.BotSecret == "" {
		return nil, errors.New("BotSecret is required")
	}
	if cfg.AuthBaseURL == "" {
		return nil, errors.New("AuthBaseURL is required")
	}
	if cfg.AuthPort <= 0 {
		cfg.AuthPort = 8787
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = "./data/bot.db"
	}
	return &Bot{cfg: cfg}, nil
}

// Run starts the Discord bot, auth HTTP server, daily scheduler, and supporting services.
func (b *Bot) Run(ctx context.Context) error {
	lock, err := acquireInstanceLock(b.cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer lock.Close()

	st, err := store.Open(b.cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()

	boxer, err := crypto.NewBoxer(b.cfg.BotSecret)
	if err != nil {
		return fmt.Errorf("crypto: %w", err)
	}

	riotClient := riot.NewClient(nil)
	if ver, err := riot.FetchClientVersion(ctx, nil); err != nil {
		log.Printf("riot client version: using default (%v)", err)
	} else {
		riotClient.ClientVersion = ver
		log.Printf("riot client version: %s", ver)
	}
	skinCache := skins.NewCache(nil, "")
	if err := skinCache.EnsureLoaded(ctx, skins.LangKO); err != nil {
		log.Printf("skins cache: load failed (shop names may be limited): %v", err)
	}

	dg, err := discordgo.New("Bot " + b.cfg.DiscordToken)
	if err != nil {
		return fmt.Errorf("discord session: %w", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages

	authServer := authweb.New(authweb.Deps{
		AuthBaseURL: b.cfg.AuthBaseURL,
		Store:       st,
		Riot:        riotClient,
		QRAuth:      riot.NewQRClient(nil),
		Boxer:       boxer,
		OnLinked: func(discordUserID, displayName string) {
			ch, err := dg.UserChannelCreate(discordUserID)
			if err != nil {
				log.Printf("auth dm channel: %v", err)
				return
			}
			_, err = dg.ChannelMessageSend(ch.ID, fmt.Sprintf("Riot 계정 연동이 완료되었습니다: **%s**\n`/shop` 으로 상점을 확인하세요.", displayName))
			if err != nil {
				log.Printf("auth dm send: %v", err)
			}
		},
	})

	addr := fmt.Sprintf("0.0.0.0:%d", b.cfg.AuthPort)
	root := http.NewServeMux()
	root.Handle("/", authServer.Handler())
	root.HandleFunc(InvitePath, inviteRedirect(b.cfg.DiscordAppID))
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: root,
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("auth http: %v", err)
		}
	}()
	warnIfLoopbackAuthBase(b.cfg.AuthBaseURL, b.cfg.AuthPort)

	shopFetcher := &bot.ShopFetcher{
		Accounts: st,
		Boxer:    boxer,
		Riot:     riotClient,
		Skins:    skinCache,
	}
	handlers := &bot.Handlers{
		Auth:     authServer,
		Accounts: st,
		Shops:    shopFetcher,
		Skins:    skinCache,
		Wishlist: st,
		Guilds:   st,
		Lang:     st,
	}
	bot.RegisterHandlers(dg, handlers)

	cmds := bot.Commands()
	appID := b.cfg.DiscordAppID

	registerGuildCmds := func(s *discordgo.Session, guildID string) {
		if guildID == "" {
			return
		}
		if _, err := s.ApplicationCommandBulkOverwrite(appID, guildID, cmds); err != nil {
			log.Printf("register commands for guild %s: %v", guildID, err)
			return
		}
		log.Printf("slash commands registered for guild %s", guildID)
	}

	dg.AddHandler(func(s *discordgo.Session, g *discordgo.GuildCreate) {
		registerGuildCmds(s, g.ID)
	})

	if err := dg.Open(); err != nil {
		return fmt.Errorf("discord open: %w", err)
	}
	defer dg.Close()

	// Clear global commands so they don't duplicate guild-scoped ones.
	if _, err := dg.ApplicationCommandBulkOverwrite(appID, "", []*discordgo.ApplicationCommand{}); err != nil {
		log.Printf("clear global commands: %v", err)
	}
	for _, g := range dg.State.Guilds {
		registerGuildCmds(dg, g.ID)
	}

	poster := &discordPoster{session: dg}
	cronExpr := b.cfg.StoreResetCron
	if cronExpr == "" {
		cronExpr = "0 0 * * *"
	}
	sched := &scheduler.Scheduler{
		Guilds:    st,
		Accounts:  st,
		Wishlists: st,
		Shops:     shopFetcher,
		Channels:  poster,
		DMs:       poster,
	}
	go func() {
		if err := sched.Start(ctx, cronExpr); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("scheduler: %v", err)
		}
	}()

	log.Printf("valorant-bot running (auth %s, discord connected, daily schedule Asia/Seoul hourly)", addr)
	log.Print(formatInviteLog(appID, b.cfg.AuthBaseURL))

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	return nil
}
