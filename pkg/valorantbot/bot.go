package valorantbot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/authweb"
	"github.com/dosfsociety/valorant-bot/internal/bot"
	appconfig "github.com/dosfsociety/valorant-bot/internal/config"
	"github.com/dosfsociety/valorant-bot/internal/crypto"
	"github.com/dosfsociety/valorant-bot/internal/netutil"
	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/scheduler"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

// Config holds settings needed to construct the bot.
// Fields mirror internal/config for public package consumers.
type Config struct {
	DiscordToken       string
	DiscordAppID       string
	DiscordGuildID     string
	BotSecret          string
	AuthPort           int
	AuthBindAddress    string
	AuthBaseURL        string
	DatabasePath       string
	StoreResetCron     string
	CaptchaBrowserMode string
	CaptchaDisplay     string
}

// Bot is the Valorant Discord store bot.
type Bot struct {
	cfg     Config
	runtime botRuntime
}

// botRuntime keeps the process-bound operations behind narrow function seams.
// Production uses the concrete adapters below; lifecycle tests replace only
// external boundaries and continue exercising Bot.Run itself. The Discord
// session is always passed by pointer because discordgo.Session owns mutexes.
type botRuntime struct {
	listen                func(network, address string) (net.Listener, error)
	fetchClientVersion    func(context.Context) (string, error)
	ensureSkinCacheLoaded func(context.Context, *skins.Cache) error
	newDiscordSession     func(token string) (*discordgo.Session, error)
	registerHandlers      func(*discordgo.Session, *bot.Handlers)
	openDiscord           func(*discordgo.Session) error
	closeDiscord          func(*discordgo.Session) error
	startScheduler        func(context.Context, *scheduler.Scheduler, string) error
	shutdownHTTP          func(*http.Server, context.Context) error
	closeHTTP             func(*http.Server) error
	shutdownHandlers      func(*bot.Handlers, context.Context) error
	closeHandlers         func(*bot.Handlers)
	shutdownAuth          func(*authweb.Server, context.Context) error
	closeAuth             func(*authweb.Server) error
	closeStore            func(*store.Store) error
}

func defaultBotRuntime() botRuntime {
	return botRuntime{
		listen:             net.Listen,
		fetchClientVersion: func(ctx context.Context) (string, error) { return riot.FetchClientVersion(ctx, nil) },
		ensureSkinCacheLoaded: func(ctx context.Context, cache *skins.Cache) error {
			return cache.EnsureLoaded(ctx, skins.LangKO)
		},
		newDiscordSession: func(token string) (*discordgo.Session, error) {
			return discordgo.New(token)
		},
		registerHandlers: bot.RegisterHandlers,
		openDiscord:      func(session *discordgo.Session) error { return session.Open() },
		closeDiscord:     func(session *discordgo.Session) error { return session.Close() },
		startScheduler: func(ctx context.Context, scheduler *scheduler.Scheduler, cronExpr string) error {
			return scheduler.Start(ctx, cronExpr)
		},
		shutdownHTTP: func(server *http.Server, ctx context.Context) error { return server.Shutdown(ctx) },
		closeHTTP:    func(server *http.Server) error { return server.Close() },
		shutdownHandlers: func(handlers *bot.Handlers, ctx context.Context) error {
			return handlers.Shutdown(ctx)
		},
		closeHandlers: func(handlers *bot.Handlers) {
			_ = handlers.Shutdown(context.Background())
		},
		shutdownAuth: func(server *authweb.Server, ctx context.Context) error {
			return server.Shutdown(ctx)
		},
		closeAuth:  func(server *authweb.Server) error { return server.Close() },
		closeStore: func(st *store.Store) error { return st.Close() },
	}
}

type trackedScheduler struct {
	done chan struct{}
	err  error
}

func startTrackedScheduler(run func() error) *trackedScheduler {
	task := &trackedScheduler{done: make(chan struct{})}
	go func() {
		task.err = run()
		close(task.done)
	}()
	return task
}

func (t *trackedScheduler) wait(ctx context.Context) error {
	select {
	case <-t.done:
		return t.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func closeRuntimeAfterScheduler(task *trackedScheduler, closeDependencies ...func()) {
	if task != nil {
		_ = task.wait(context.Background())
	}
	for _, closeDependency := range closeDependencies {
		closeDependency()
	}
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
	bindAddress, err := appconfig.NormalizeAuthBindAddress(cfg.AuthBindAddress)
	if err != nil {
		return nil, err
	}
	cfg.AuthBindAddress = bindAddress
	mode, err := netutil.NormalizeCaptchaBrowserMode(cfg.CaptchaBrowserMode, cfg.AuthBaseURL)
	if err != nil {
		return nil, err
	}
	cfg.CaptchaBrowserMode = string(mode)
	if cfg.AuthPort <= 0 {
		cfg.AuthPort = 8787
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = "./data/bot.db"
	}
	if cfg.CaptchaDisplay == "" {
		cfg.CaptchaDisplay = ":99"
	}
	return &Bot{cfg: cfg, runtime: defaultBotRuntime()}, nil
}

// Run starts the Discord bot, auth HTTP server, daily scheduler, and supporting services.
func (b *Bot) Run(ctx context.Context) error {
	runtime := b.runtime
	if runtime.listen == nil {
		runtime = defaultBotRuntime()
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	lock, err := acquireInstanceLock(b.cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer lock.Close()

	addr := authListenAddress(b.cfg.AuthBindAddress, b.cfg.AuthPort)
	listener, err := runtime.listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("auth http listen %s: %w", addr, err)
	}
	listenerTransferred := false
	defer func() {
		if !listenerTransferred {
			_ = listener.Close()
		}
	}()

	st, err := store.Open(b.cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}

	var (
		authServer    *authweb.Server
		handlers      *bot.Handlers
		httpSrv       *http.Server
		dg            *discordgo.Session
		discordOpen   bool
		schedulerTask *trackedScheduler
	)
	// A scheduler that outlives the bounded shutdown still joins before the
	// HTTP, interaction, auth, Discord, and store dependencies disappear.
	defer func() {
		closeRuntimeAfterScheduler(
			schedulerTask,
			func() {
				if httpSrv != nil {
					_ = runtime.closeHTTP(httpSrv)
				}
			},
			func() {
				if handlers != nil {
					runtime.closeHandlers(handlers)
				}
			},
			func() {
				if authServer != nil {
					_ = runtime.closeAuth(authServer)
				}
			},
			func() {
				if discordOpen {
					_ = runtime.closeDiscord(dg)
				}
			},
			func() { _ = runtime.closeStore(st) },
		)
	}()

	boxer, err := crypto.NewBoxer(b.cfg.BotSecret)
	if err != nil {
		return fmt.Errorf("crypto: %w", err)
	}

	riotClient := riot.NewClient(nil)
	if ver, err := runtime.fetchClientVersion(runCtx); err != nil {
		log.Printf("riot client version: using default (%v)", err)
	} else {
		riotClient.ClientVersion = ver
		log.Printf("riot client version: %s", ver)
	}
	skinCache := skins.NewCache(nil, "")
	if err := runtime.ensureSkinCacheLoaded(runCtx, skinCache); err != nil {
		log.Printf("skins cache: load failed (shop names may be limited): %v", err)
	}

	dg, err = runtime.newDiscordSession("Bot " + b.cfg.DiscordToken)
	if err != nil {
		return fmt.Errorf("discord session: %w", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages

	authServer = authweb.New(authweb.Deps{
		AuthBaseURL:        b.cfg.AuthBaseURL,
		CaptchaBrowserMode: netutil.CaptchaBrowserMode(b.cfg.CaptchaBrowserMode),
		CaptchaDisplay:     b.cfg.CaptchaDisplay,
		Store:              st,
		Riot:               riotClient,
		QRAuth:             riot.NewQRClient(nil),
		PasswordAuth:       riot.NewPasswordClient(nil),
		Boxer:              boxer,
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

	root := http.NewServeMux()
	root.Handle("/", authServer.Handler())
	root.HandleFunc(InvitePath, inviteRedirect(b.cfg.DiscordAppID))
	httpSrv = &http.Server{
		Addr:    addr,
		Handler: root,
	}
	warnIfLoopbackAuthBase(b.cfg.AuthBaseURL, b.cfg.AuthPort)

	shopFetcher := &bot.ShopFetcher{
		Accounts: st,
		Boxer:    boxer,
		Riot:     riotClient,
		Skins:    skinCache,
	}
	handlers = &bot.Handlers{
		Auth:     authServer,
		Accounts: st,
		Shops:    shopFetcher,
		Skins:    skinCache,
		Wishlist: st,
		Guilds:   st,
		Lang:     st,
	}
	runtime.registerHandlers(dg, handlers)

	cmds := bot.Commands()
	appID := b.cfg.DiscordAppID

	registerGuildCmds := func(s *discordgo.Session, guildID string) {
		if guildID == "" || runCtx.Err() != nil {
			return
		}
		if _, err := s.ApplicationCommandBulkOverwrite(appID, guildID, cmds, discordgo.WithContext(runCtx)); err != nil {
			if runCtx.Err() != nil {
				return
			}
			log.Printf("register commands for guild %s: %v", guildID, err)
			return
		}
		log.Printf("slash commands registered for guild %s", guildID)
	}

	dg.AddHandler(func(s *discordgo.Session, g *discordgo.GuildCreate) {
		registerGuildCmds(s, g.ID)
	})

	if err := runtime.openDiscord(dg); err != nil {
		return fmt.Errorf("discord open: %w", err)
	}
	discordOpen = true

	serveDone := make(chan error, 1)
	listenerTransferred = true
	go func() {
		serveDone <- httpSrv.Serve(listener)
		// Serve owns and closes the listener. Any return before the parent asks
		// us to stop is terminal, so abort context-aware startup work at once.
		cancelRun()
	}()

	// Clear global commands so they don't duplicate guild-scoped ones.
	if _, err := dg.ApplicationCommandBulkOverwrite(
		appID,
		"",
		[]*discordgo.ApplicationCommand{},
		discordgo.WithContext(runCtx),
	); err != nil && runCtx.Err() == nil {
		log.Printf("clear global commands: %v", err)
	}
	for _, g := range dg.State.Guilds {
		if runCtx.Err() != nil {
			break
		}
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
	var runErr error
	intendedShutdown := false
	select {
	case <-ctx.Done():
		intendedShutdown = true
	case serveErr := <-serveDone:
		runErr = unexpectedHTTPServeError(serveErr)
	default:
		schedulerTask = startTrackedScheduler(func() error {
			err := runtime.startScheduler(runCtx, sched, cronExpr)
			if runCtx.Err() != nil && errors.Is(err, runCtx.Err()) {
				return nil
			}
			if err != nil {
				log.Printf("scheduler: %v", err)
			}
			return err
		})

		log.Printf("valorant-bot running (auth %s, discord connected, daily schedule Asia/Seoul hourly)", addr)
		log.Print(formatInviteLog(appID, b.cfg.AuthBaseURL))

		select {
		case <-ctx.Done():
			intendedShutdown = true
		case serveErr := <-serveDone:
			runErr = unexpectedHTTPServeError(serveErr)
		}
	}
	cancelRun()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpDone := make(chan error, 1)
	authDone := make(chan error, 1)
	handlerDone := make(chan error, 1)
	schedulerDone := make(chan error, 1)
	go func() { httpDone <- runtime.shutdownHTTP(httpSrv, shutdownCtx) }()
	go func() { authDone <- runtime.shutdownAuth(authServer, shutdownCtx) }()
	go func() { handlerDone <- runtime.shutdownHandlers(handlers, shutdownCtx) }()
	if schedulerTask == nil {
		schedulerDone <- nil
	} else {
		go func() { schedulerDone <- schedulerTask.wait(shutdownCtx) }()
	}
	shutdownErr := errors.Join(<-httpDone, <-handlerDone, <-authDone, <-schedulerDone)
	if intendedShutdown {
		serveErr := <-serveDone
		if !errors.Is(serveErr, http.ErrServerClosed) {
			runErr = unexpectedHTTPServeError(serveErr)
		}
	}
	return errors.Join(runErr, shutdownErr)
}

func unexpectedHTTPServeError(err error) error {
	if err == nil {
		return errors.New("auth http serve: stopped unexpectedly")
	}
	return fmt.Errorf("auth http serve: %w", err)
}

func authListenAddress(bindAddress string, port int) string {
	return net.JoinHostPort(bindAddress, strconv.Itoa(port))
}
