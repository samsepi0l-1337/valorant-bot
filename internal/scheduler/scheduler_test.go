package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/bot"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

type fakeGuilds struct {
	settings []store.GuildSettings
	err      error
}

func (f *fakeGuilds) ListEnabledGuildSettings() ([]store.GuildSettings, error) {
	return f.settings, f.err
}

type fakeAccounts struct {
	accounts []store.Account
	err      error
}

func (f *fakeAccounts) ListAllRiotAccounts() ([]store.Account, error) {
	return f.accounts, f.err
}

type fakeWishlists struct {
	items []store.WishlistItem
	err   error
}

func (f *fakeWishlists) ListAllWishlists() ([]store.WishlistItem, error) {
	return f.items, f.err
}

type fakeShops struct {
	byUser map[string][]bot.AccountShop
	calls  []string
	mu     sync.Mutex
}

func (f *fakeShops) ShopsForUser(ctx context.Context, discordUserID, language string) ([]bot.AccountShop, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, discordUserID)
	return f.byUser[discordUserID], nil
}

type channelPost struct {
	ChannelID string
	Content   string
	Embeds    []*discordgo.MessageEmbed
}

type fakeChannels struct {
	posts []channelPost
	mu    sync.Mutex
}

func (f *fakeChannels) PostChannel(ctx context.Context, channelID, content string, embeds []*discordgo.MessageEmbed) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, channelPost{ChannelID: channelID, Content: content, Embeds: embeds})
	return nil
}

type dmPost struct {
	UserID  string
	Content string
	Embeds  []*discordgo.MessageEmbed
}

type fakeDMs struct {
	posts []dmPost
	mu    sync.Mutex
}

func (f *fakeDMs) SendDM(ctx context.Context, discordUserID, content string, embeds []*discordgo.MessageEmbed) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, dmPost{UserID: discordUserID, Content: content, Embeds: embeds})
	return nil
}

func TestRunOnce_PostsToEnabledGuildChannels(t *testing.T) {
	channels := &fakeChannels{}
	s := &Scheduler{
		Guilds: &fakeGuilds{settings: []store.GuildSettings{
			{GuildID: "g1", DailyChannelID: "chan-1", Enabled: true},
			{GuildID: "g2", DailyChannelID: "chan-2", Enabled: true},
		}},
		Accounts: &fakeAccounts{accounts: []store.Account{
			{DiscordUserID: "user-a", PUUID: "p1", GameName: "Alice", TagLine: "NA1"},
		}},
		Wishlists: &fakeWishlists{},
		Shops: &fakeShops{byUser: map[string][]bot.AccountShop{
			"user-a": {{
				GameName: "Alice", TagLine: "NA1",
				Offers: []bot.OfferView{{DisplayName: "Reaver", SkinUUID: "skin-1", CostVP: 1775}},
			}},
		}},
		Channels: channels,
		DMs:      &fakeDMs{},
	}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(channels.posts) != 2 {
		t.Fatalf("channel posts = %d, want 2", len(channels.posts))
	}
	ids := map[string]bool{}
	for _, p := range channels.posts {
		ids[p.ChannelID] = true
		if len(p.Embeds) == 0 {
			t.Errorf("expected embeds for channel %s", p.ChannelID)
		}
	}
	if !ids["chan-1"] || !ids["chan-2"] {
		t.Fatalf("posted channels = %v", ids)
	}
}

func TestRunOnce_SkipsGuildsWithEmptyChannel(t *testing.T) {
	channels := &fakeChannels{}
	s := &Scheduler{
		Guilds: &fakeGuilds{settings: []store.GuildSettings{
			{GuildID: "g1", DailyChannelID: "", Enabled: true},
			{GuildID: "g2", DailyChannelID: "chan-ok", Enabled: true},
		}},
		Accounts:  &fakeAccounts{},
		Wishlists: &fakeWishlists{},
		Shops:     &fakeShops{byUser: map[string][]bot.AccountShop{}},
		Channels:  channels,
		DMs:       &fakeDMs{},
	}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(channels.posts) != 1 {
		t.Fatalf("channel posts = %d, want 1", len(channels.posts))
	}
	if channels.posts[0].ChannelID != "chan-ok" {
		t.Fatalf("got channel %q", channels.posts[0].ChannelID)
	}
}

func TestRunOnce_WishlistMatchSendsDM(t *testing.T) {
	dms := &fakeDMs{}
	s := &Scheduler{
		Guilds:    &fakeGuilds{},
		Accounts:  &fakeAccounts{accounts: []store.Account{{DiscordUserID: "user-a", PUUID: "p1"}}},
		Wishlists: &fakeWishlists{items: []store.WishlistItem{
			{DiscordUserID: "user-a", SkinUUID: "skin-hit", SkinName: "Reaver Vandal"},
		}},
		Shops: &fakeShops{byUser: map[string][]bot.AccountShop{
			"user-a": {{
				Offers: []bot.OfferView{
					{DisplayName: "Other", SkinUUID: "skin-other"},
					{DisplayName: "Reaver Vandal", SkinUUID: "skin-hit"},
				},
			}},
		}},
		Channels: &fakeChannels{},
		DMs:      dms,
	}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(dms.posts) != 1 {
		t.Fatalf("DM posts = %d, want 1", len(dms.posts))
	}
	if dms.posts[0].UserID != "user-a" {
		t.Errorf("DM user = %q", dms.posts[0].UserID)
	}
	if dms.posts[0].Content == "" {
		t.Error("expected non-empty DM content")
	}
}

func TestRunOnce_NoWishlistMatchMeansNoDM(t *testing.T) {
	dms := &fakeDMs{}
	s := &Scheduler{
		Guilds:    &fakeGuilds{},
		Accounts:  &fakeAccounts{accounts: []store.Account{{DiscordUserID: "user-a", PUUID: "p1"}}},
		Wishlists: &fakeWishlists{items: []store.WishlistItem{
			{DiscordUserID: "user-a", SkinUUID: "skin-miss", SkinName: "Prime"},
		}},
		Shops: &fakeShops{byUser: map[string][]bot.AccountShop{
			"user-a": {{Offers: []bot.OfferView{{SkinUUID: "skin-other"}}}},
		}},
		Channels: &fakeChannels{},
		DMs:      dms,
	}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(dms.posts) != 0 {
		t.Fatalf("DM posts = %d, want 0", len(dms.posts))
	}
}

func TestRunOnce_DedupsWishlistDMPerUserSkin(t *testing.T) {
	dms := &fakeDMs{}
	s := &Scheduler{
		Guilds:   &fakeGuilds{},
		Accounts: &fakeAccounts{accounts: []store.Account{{DiscordUserID: "user-a", PUUID: "p1"}}},
		Wishlists: &fakeWishlists{items: []store.WishlistItem{
			{DiscordUserID: "user-a", SkinUUID: "skin-hit", SkinName: "Reaver"},
			{DiscordUserID: "user-a", SkinUUID: "skin-hit", SkinName: "Reaver"},
		}},
		Shops: &fakeShops{byUser: map[string][]bot.AccountShop{
			"user-a": {
				{Offers: []bot.OfferView{{SkinUUID: "skin-hit"}}},
				{Offers: []bot.OfferView{{SkinUUID: "skin-hit"}}},
			},
		}},
		Channels: &fakeChannels{},
		DMs:      dms,
	}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(dms.posts) != 1 {
		t.Fatalf("DM posts = %d, want 1 (deduped)", len(dms.posts))
	}
}

func TestStart_RunsOnCron(t *testing.T) {
	channels := &fakeChannels{}
	s := &Scheduler{
		Guilds: &fakeGuilds{settings: []store.GuildSettings{
			{GuildID: "g1", DailyChannelID: "chan-1", Enabled: true},
		}},
		Accounts:  &fakeAccounts{},
		Wishlists: &fakeWishlists{},
		Shops:     &fakeShops{byUser: map[string][]bot.AccountShop{}},
		Channels:  channels,
		DMs:       &fakeDMs{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx, "@every 1s")
	}()

	deadline := time.After(3 * time.Second)
	for {
		channels.mu.Lock()
		n := len(channels.posts)
		channels.mu.Unlock()
		if n >= 1 {
			cancel()
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("timed out waiting for cron RunOnce")
		case err := <-errCh:
			if err != nil && ctx.Err() == nil {
				t.Fatalf("Start: %v", err)
			}
			t.Fatal("Start exited before posting")
		case <-time.After(50 * time.Millisecond):
		}
	}

	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}
