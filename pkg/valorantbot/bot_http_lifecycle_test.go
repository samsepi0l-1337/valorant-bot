package valorantbot

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/authweb"
	internalbot "github.com/dosfsociety/valorant-bot/internal/bot"
	"github.com/dosfsociety/valorant-bot/internal/scheduler"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

const lifecycleTestTimeout = 3 * time.Second

type controlledListener struct {
	started   chan struct{}
	closed    chan struct{}
	fail      chan error
	mu        sync.Mutex
	closes    int
	startOnce sync.Once
	closeOnce sync.Once
}

func newControlledListener() *controlledListener {
	return &controlledListener{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		fail:    make(chan error, 1),
	}
}

func (l *controlledListener) Accept() (net.Conn, error) {
	l.startOnce.Do(func() { close(l.started) })
	select {
	case err := <-l.fail:
		return nil, err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *controlledListener) Close() error {
	l.mu.Lock()
	l.closes++
	l.mu.Unlock()
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *controlledListener) Addr() net.Addr {
	return lifecycleTestAddr("127.0.0.1:45678")
}

func (l *controlledListener) failWith(err error) {
	l.fail <- err
}

func (l *controlledListener) closeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closes
}

type lifecycleTestAddr string

func (a lifecycleTestAddr) Network() string { return "tcp" }
func (a lifecycleTestAddr) String() string  { return string(a) }

type lifecycleProbe struct {
	mu                 sync.Mutex
	counts             map[string]int
	discordOpened      chan struct{}
	discordClosed      chan struct{}
	schedulerStarted   chan struct{}
	schedulerCanceled  chan struct{}
	discordOpenOnce    sync.Once
	discordCloseOnce   sync.Once
	schedulerStartOnce sync.Once
	schedulerStopOnce  sync.Once
}

func newLifecycleProbe() *lifecycleProbe {
	return &lifecycleProbe{
		counts:            make(map[string]int),
		discordOpened:     make(chan struct{}),
		discordClosed:     make(chan struct{}),
		schedulerStarted:  make(chan struct{}),
		schedulerCanceled: make(chan struct{}),
	}
}

func (p *lifecycleProbe) record(name string) {
	p.mu.Lock()
	p.counts[name]++
	p.mu.Unlock()
}

func (p *lifecycleProbe) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[name]
}

func waitForLifecycleEvent(t *testing.T, event <-chan struct{}, name string) {
	t.Helper()
	timer := time.NewTimer(lifecycleTestTimeout)
	defer timer.Stop()
	select {
	case <-event:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForRunResult(t *testing.T, result <-chan error) error {
	t.Helper()
	timer := time.NewTimer(lifecycleTestTimeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		t.Fatal("timed out waiting for Bot.Run")
		return nil
	}
}

type lifecycleDiscordTransport struct{}

func (lifecycleDiscordTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("[]")),
	}, nil
}

func newLifecycleDiscordSession(t *testing.T) *discordgo.Session {
	t.Helper()
	session, err := discordgo.New("Bot lifecycle-test")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	session.Client = &http.Client{Transport: lifecycleDiscordTransport{}}
	return session
}

func lifecycleTestBot(t *testing.T, listener net.Listener) (*Bot, *lifecycleProbe, *discordgo.Session) {
	t.Helper()
	b, err := New(Config{
		DiscordToken:    "token",
		DiscordAppID:    "app-id",
		BotSecret:       "0123456789abcdef0123456789abcdef",
		AuthPort:        45678,
		AuthBindAddress: "127.0.0.1",
		AuthBaseURL:     "http://127.0.0.1:45678",
		DatabasePath:    t.TempDir() + "/bot.db",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	probe := newLifecycleProbe()
	session := newLifecycleDiscordSession(t)
	runtime := defaultBotRuntime()
	runtime.listen = func(network, address string) (net.Listener, error) {
		probe.record("listen")
		if network != "tcp" {
			return nil, errors.New("unexpected network: " + network)
		}
		if address != "127.0.0.1:45678" {
			return nil, errors.New("unexpected address: " + address)
		}
		return listener, nil
	}
	runtime.fetchClientVersion = func(context.Context) (string, error) {
		return "test-version", nil
	}
	runtime.ensureSkinCacheLoaded = func(context.Context, *skins.Cache) error { return nil }
	runtime.newDiscordSession = func(string) (*discordgo.Session, error) {
		probe.record("discord-new")
		return session, nil
	}
	runtime.registerHandlers = func(session *discordgo.Session, handlers *internalbot.Handlers) {
		probe.record("handlers-register")
		internalbot.RegisterHandlers(session, handlers)
	}
	runtime.openDiscord = func(*discordgo.Session) error {
		probe.record("discord-open")
		probe.discordOpenOnce.Do(func() { close(probe.discordOpened) })
		return nil
	}
	runtime.closeDiscord = func(*discordgo.Session) error {
		probe.record("discord-close")
		probe.discordCloseOnce.Do(func() { close(probe.discordClosed) })
		return nil
	}
	runtime.startScheduler = func(ctx context.Context, _ *scheduler.Scheduler, _ string) error {
		probe.record("scheduler-start")
		probe.schedulerStartOnce.Do(func() { close(probe.schedulerStarted) })
		<-ctx.Done()
		probe.record("scheduler-cancel")
		probe.schedulerStopOnce.Do(func() { close(probe.schedulerCanceled) })
		return ctx.Err()
	}
	runtime.shutdownHTTP = func(server *http.Server, ctx context.Context) error {
		probe.record("http-shutdown")
		return server.Shutdown(ctx)
	}
	runtime.closeHTTP = func(server *http.Server) error {
		probe.record("http-close")
		return server.Close()
	}
	runtime.shutdownHandlers = func(handlers *internalbot.Handlers, ctx context.Context) error {
		probe.record("handlers-shutdown")
		return handlers.Shutdown(ctx)
	}
	runtime.closeHandlers = func(handlers *internalbot.Handlers) {
		probe.record("handlers-close")
		_ = handlers.Shutdown(context.Background())
	}
	runtime.shutdownAuth = func(server *authweb.Server, ctx context.Context) error {
		probe.record("auth-shutdown")
		return server.Shutdown(ctx)
	}
	runtime.closeAuth = func(server *authweb.Server) error {
		probe.record("auth-close")
		return server.Close()
	}
	runtime.closeStore = func(st *store.Store) error {
		probe.record("store-close")
		return st.Close()
	}
	b.runtime = runtime
	return b, probe, session
}

func assertLifecycleCleanup(t *testing.T, probe *lifecycleProbe) {
	t.Helper()
	for _, event := range []string{
		"handlers-shutdown",
		"auth-shutdown",
		"http-shutdown",
		"handlers-close",
		"auth-close",
		"http-close",
		"discord-close",
		"store-close",
		"scheduler-cancel",
	} {
		if got := probe.count(event); got != 1 {
			t.Errorf("%s count = %d, want 1", event, got)
		}
	}
}

// Mutation caught: moving net.Listen back into the HTTP goroutine permits
// Discord startup and handler registration while the configured port is busy.
func TestRunFailsBeforeDiscordStartupWhenAuthAddressIsOccupied(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy loopback port: %v", err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	b, err := New(Config{
		DiscordToken:    "token",
		DiscordAppID:    "app-id",
		BotSecret:       "0123456789abcdef0123456789abcdef",
		AuthPort:        port,
		AuthBindAddress: "127.0.0.1",
		AuthBaseURL:     "http://127.0.0.1:" + strconv.Itoa(port),
		DatabasePath:    t.TempDir() + "/bot.db",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	probe := newLifecycleProbe()
	runtime := defaultBotRuntime()
	runtime.newDiscordSession = func(string) (*discordgo.Session, error) {
		probe.record("discord-new")
		return nil, errors.New("discord must not be constructed")
	}
	runtime.registerHandlers = func(*discordgo.Session, *internalbot.Handlers) {
		probe.record("handlers-register")
	}
	runtime.openDiscord = func(*discordgo.Session) error {
		probe.record("discord-open")
		return errors.New("discord must not open")
	}
	b.runtime = runtime

	err = b.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with occupied auth address")
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	if !strings.Contains(err.Error(), "auth http listen "+addr) {
		t.Fatalf("Run error = %q, want bind context for %s", err, addr)
	}
	for _, event := range []string{"discord-new", "handlers-register", "discord-open"} {
		if got := probe.count(event); got != 0 {
			t.Errorf("%s count = %d, want 0", event, got)
		}
	}
}

func TestRunReturnsInvalidAuthListenAddressBeforeDiscordStartup(t *testing.T) {
	b, err := New(Config{
		DiscordToken:    "token",
		DiscordAppID:    "app-id",
		BotSecret:       "0123456789abcdef0123456789abcdef",
		AuthPort:        70000,
		AuthBindAddress: "127.0.0.1",
		AuthBaseURL:     "http://127.0.0.1:70000",
		DatabasePath:    t.TempDir() + "/bot.db",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	probe := newLifecycleProbe()
	runtime := defaultBotRuntime()
	runtime.newDiscordSession = func(string) (*discordgo.Session, error) {
		probe.record("discord-new")
		return nil, errors.New("discord must not be constructed")
	}
	runtime.registerHandlers = func(*discordgo.Session, *internalbot.Handlers) {
		probe.record("handlers-register")
	}
	runtime.openDiscord = func(*discordgo.Session) error {
		probe.record("discord-open")
		return errors.New("discord must not open")
	}
	b.runtime = runtime

	err = b.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "auth http listen 127.0.0.1:70000") {
		t.Fatalf("Run error = %v, want invalid auth listen address", err)
	}
	for _, event := range []string{"discord-new", "handlers-register", "discord-open"} {
		if got := probe.count(event); got != 0 {
			t.Errorf("%s count = %d, want 0", event, got)
		}
	}
}

// Mutation caught: logging and dropping Server.Serve errors leaves Run alive
// and its Discord/auth/store dependencies open after the listener dies.
func TestRunPropagatesPostStartHTTPServeFailureAndCleansUp(t *testing.T) {
	listener := newControlledListener()
	b, probe, _ := lifecycleTestBot(t, listener)
	result := make(chan error, 1)
	go func() { result <- b.Run(context.Background()) }()

	waitForLifecycleEvent(t, probe.discordOpened, "Discord open")
	waitForLifecycleEvent(t, listener.started, "HTTP Serve Accept")
	waitForLifecycleEvent(t, probe.schedulerStarted, "scheduler start")

	serveFailure := errors.New("forced listener failure")
	listener.failWith(serveFailure)
	err := waitForRunResult(t, result)
	if !errors.Is(err, serveFailure) {
		t.Fatalf("Run error = %v, want forced listener failure", err)
	}
	waitForLifecycleEvent(t, probe.schedulerCanceled, "scheduler cancellation")
	assertLifecycleCleanup(t, probe)
	if got := listener.closeCount(); got != 1 {
		t.Fatalf("listener close count = %d, want exactly 1", got)
	}
}

// Mutation caught: treating http.ErrServerClosed as a runtime failure makes a
// deliberate context cancellation return a spurious error.
func TestRunCancellationTreatsHTTPServerClosedAsCleanShutdown(t *testing.T) {
	listener := newControlledListener()
	b, probe, _ := lifecycleTestBot(t, listener)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- b.Run(ctx) }()

	waitForLifecycleEvent(t, probe.discordOpened, "Discord open")
	waitForLifecycleEvent(t, listener.started, "HTTP Serve Accept")
	waitForLifecycleEvent(t, probe.schedulerStarted, "scheduler start")
	cancel()

	if err := waitForRunResult(t, result); err != nil {
		t.Fatalf("Run cancellation error = %v, want nil", err)
	}
	waitForLifecycleEvent(t, probe.schedulerCanceled, "scheduler cancellation")
	assertLifecycleCleanup(t, probe)
	if got := listener.closeCount(); got != 1 {
		t.Fatalf("listener close count = %d, want exactly 1", got)
	}
}

type blockingCommandTransport struct {
	started      chan struct{}
	canceled     chan struct{}
	release      chan struct{}
	startOnce    sync.Once
	canceledOnce sync.Once
}

func newBlockingCommandTransport() *blockingCommandTransport {
	return &blockingCommandTransport{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (transport *blockingCommandTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.startOnce.Do(func() { close(transport.started) })
	select {
	case <-request.Context().Done():
		transport.canceledOnce.Do(func() { close(transport.canceled) })
		return nil, request.Context().Err()
	case <-transport.release:
		return nil, errors.New("blocking transport released")
	}
}

// Mutation caught: waiting to observe Serve until after synchronous Discord
// command registration leaves the session live for the REST request timeout.
func TestRunServeFailureCancelsBlockedDiscordCommandRegistration(t *testing.T) {
	listener := newControlledListener()
	b, probe, session := lifecycleTestBot(t, listener)
	transport := newBlockingCommandTransport()
	defer close(transport.release)
	session.Client = &http.Client{Transport: transport}

	result := make(chan error, 1)
	go func() { result <- b.Run(context.Background()) }()
	waitForLifecycleEvent(t, listener.started, "HTTP Serve Accept")
	waitForLifecycleEvent(t, transport.started, "Discord command registration")

	serveFailure := errors.New("listener failed during command registration")
	listener.failWith(serveFailure)
	waitForLifecycleEvent(t, transport.canceled, "command registration cancellation")
	waitForLifecycleEvent(t, probe.discordClosed, "Discord cleanup")
	if err := waitForRunResult(t, result); !errors.Is(err, serveFailure) {
		t.Fatalf("Run error = %v, want listener failure", err)
	}
	if got := probe.count("scheduler-start"); got != 0 {
		t.Fatalf("scheduler start count = %d, want 0 after startup Serve failure", got)
	}
	if got := listener.closeCount(); got != 1 {
		t.Fatalf("listener close count = %d, want exactly 1", got)
	}
}
