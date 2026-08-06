package bot_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/bot"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

type fakeSkins struct {
	results []skins.Skin
	byUUID  map[string]skins.Skin
}

func (f *fakeSkins) EnsureLoaded(ctx context.Context, language string) error { return nil }

func (f *fakeSkins) SearchByName(query, language string) []skins.Skin {
	return f.results
}

func (f *fakeSkins) Get(uuid, language string) (skins.Skin, bool) {
	if f.byUUID != nil {
		s, ok := f.byUUID[uuid]
		return s, ok
	}
	for _, s := range f.results {
		if s.UUID == uuid {
			return s, true
		}
	}
	return skins.Skin{}, false
}

type memWishlist struct {
	items map[string][]store.WishlistItem
}

func (m *memWishlist) AddWishlist(discordUserID, skinUUID, skinName string) error {
	m.items[discordUserID] = append(m.items[discordUserID], store.WishlistItem{
		DiscordUserID: discordUserID,
		SkinUUID:      skinUUID,
		SkinName:      skinName,
	})
	return nil
}

func (m *memWishlist) RemoveWishlist(discordUserID, skinUUID string) error {
	list := m.items[discordUserID]
	var next []store.WishlistItem
	for _, w := range list {
		if w.SkinUUID != skinUUID {
			next = append(next, w)
		}
	}
	m.items[discordUserID] = next
	return nil
}

func (m *memWishlist) ListWishlists(discordUserID string) ([]store.WishlistItem, error) {
	return append([]store.WishlistItem(nil), m.items[discordUserID]...), nil
}

type memGuild struct {
	settings map[string]store.GuildSettings
}

func (m *memGuild) UpsertGuildSettings(gs store.GuildSettings) error {
	m.settings[gs.GuildID] = gs
	return nil
}

func (m *memGuild) GetGuildSettings(guildID string) (store.GuildSettings, bool, error) {
	gs, ok := m.settings[guildID]
	return gs, ok, nil
}

func TestHandleWishlistAdd_ShowsSelectMenu(t *testing.T) {
	wl := &memWishlist{items: map[string][]store.WishlistItem{}}
	h := &bot.Handlers{
		Skins: &fakeSkins{results: []skins.Skin{
			{UUID: "a", DisplayName: "프라임 밴달"},
			{UUID: "b", DisplayName: "프라임 클래식"},
		}},
		Wishlist: wl,
	}
	resp, err := h.HandleWishlistAdd("u1", "프라임", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "2") || !strings.Contains(resp.Content, "선택") {
		t.Fatalf("content %q", resp.Content)
	}
	if len(resp.Components) == 0 {
		t.Fatal("expected select menu")
	}
	row, ok := resp.Components[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("row %T", resp.Components[0])
	}
	menu, ok := row.Components[0].(discordgo.SelectMenu)
	if !ok {
		t.Fatalf("menu %T", row.Components[0])
	}
	if !strings.HasPrefix(menu.CustomID, "wishlist:add:u1") {
		t.Fatalf("customID %q", menu.CustomID)
	}
	if len(menu.Options) != 2 {
		t.Fatalf("options %d", len(menu.Options))
	}
	list, _ := wl.ListWishlists("u1")
	if len(list) != 0 {
		t.Fatal("should not add until select")
	}
}

func TestHandleWishlistSelectAdd(t *testing.T) {
	wl := &memWishlist{items: map[string][]store.WishlistItem{}}
	h := &bot.Handlers{
		Skins: &fakeSkins{results: []skins.Skin{
			{UUID: "uuid-1", DisplayName: "프라임 밴달"},
		}},
		Wishlist: wl,
	}
	resp, err := h.HandleWishlistSelectAdd("u1", "uuid-1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "프라임 밴달") || !strings.Contains(resp.Content, "추가") {
		t.Fatalf("content %q", resp.Content)
	}
	list, _ := wl.ListWishlists("u1")
	if len(list) != 1 || list[0].SkinUUID != "uuid-1" {
		t.Fatalf("wishlist %+v", list)
	}
}

func TestHandleWishlistAdd_NotFound(t *testing.T) {
	h := &bot.Handlers{
		Skins:    &fakeSkins{results: nil},
		Wishlist: &memWishlist{items: map[string][]store.WishlistItem{}},
	}
	resp, err := h.HandleWishlistAdd("u1", "nope", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "찾을 수 없") {
		t.Fatalf("content %q", resp.Content)
	}
}

func TestHandleWishlistRemove_ByUUID(t *testing.T) {
	wl := &memWishlist{items: map[string][]store.WishlistItem{
		"u1": {{SkinUUID: "uuid-del", SkinName: "X"}},
	}}
	h := &bot.Handlers{Wishlist: wl}
	resp, err := h.HandleWishlistRemove("u1", "uuid-del", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "제거") {
		t.Fatalf("content %q", resp.Content)
	}
	list, _ := wl.ListWishlists("u1")
	if len(list) != 0 {
		t.Fatal("expected empty")
	}
}

func TestHandleWishlistRemove_AmbiguousSelect(t *testing.T) {
	wl := &memWishlist{items: map[string][]store.WishlistItem{
		"u1": {
			{SkinUUID: "a", SkinName: "프라임 밴달"},
			{SkinUUID: "b", SkinName: "프라임 클래식"},
		},
	}}
	h := &bot.Handlers{Wishlist: wl}
	resp, err := h.HandleWishlistRemove("u1", "프라임", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Components) == 0 {
		t.Fatal("expected select")
	}
	list, _ := wl.ListWishlists("u1")
	if len(list) != 2 {
		t.Fatal("should not remove until select")
	}
}

func TestHandleWishlistRemove_ByName(t *testing.T) {
	wl := &memWishlist{items: map[string][]store.WishlistItem{
		"u1": {{SkinUUID: "u", SkinName: "Prime Vandal"}},
	}}
	h := &bot.Handlers{Wishlist: wl}
	_, err := h.HandleWishlistRemove("u1", "prime", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := wl.ListWishlists("u1")
	if len(list) != 0 {
		t.Fatal("expected removed")
	}
}

func TestHandleWishlistList(t *testing.T) {
	h := &bot.Handlers{
		Wishlist: &memWishlist{items: map[string][]store.WishlistItem{
			"u1": {{SkinName: "Prime Vandal", SkinUUID: "p1"}},
		}},
	}
	resp, err := h.HandleWishlistList("u1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "Prime Vandal") {
		t.Fatalf("content %q", resp.Content)
	}
}

func TestHandleChannelSet(t *testing.T) {
	gs := &memGuild{settings: map[string]store.GuildSettings{}}
	h := &bot.Handlers{Guilds: gs}
	resp, err := h.HandleChannelSet("guild-1", "chan-99", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "설정") {
		t.Fatalf("content %q", resp.Content)
	}
	got := gs.settings["guild-1"]
	if got.DailyChannelID != "chan-99" || !got.Enabled {
		t.Fatalf("settings %+v", got)
	}
	if got.DailyHour != store.DefaultDailyHourKST {
		t.Fatalf("hour %d", got.DailyHour)
	}
}

func TestHandleChannelTimeMenuAndSelect(t *testing.T) {
	gs := &memGuild{settings: map[string]store.GuildSettings{
		"guild-1": {GuildID: "guild-1", DailyChannelID: "c1", Enabled: true, DailyHour: 9},
	}}
	h := &bot.Handlers{Guilds: gs}
	menu, err := h.HandleChannelTimeMenu("guild-1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if len(menu.Components) == 0 {
		t.Fatal("expected select")
	}
	resp, err := h.HandleChannelTimeSelect("guild-1", "21", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "21:00") {
		t.Fatalf("content %q", resp.Content)
	}
	if gs.settings["guild-1"].DailyHour != 21 {
		t.Fatalf("%+v", gs.settings["guild-1"])
	}
	if gs.settings["guild-1"].DailyChannelID != "c1" {
		t.Fatal("channel should be preserved")
	}
}
