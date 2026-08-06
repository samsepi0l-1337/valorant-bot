package bot_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/bot"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

type fakeAuth struct {
	url   string
	state string
	err   error
}

func (f *fakeAuth) BeginAuth(discordUserID string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.url, f.state, nil
}

type memAccounts struct {
	byUser map[string][]store.Account
}

func (m *memAccounts) ListRiotAccountsByDiscord(discordUserID string) ([]store.Account, error) {
	return append([]store.Account(nil), m.byUser[discordUserID]...), nil
}

func (m *memAccounts) DeleteRiotAccount(discordUserID, puuid string) error {
	list := m.byUser[discordUserID]
	var next []store.Account
	for _, a := range list {
		if a.PUUID != puuid {
			next = append(next, a)
		}
	}
	m.byUser[discordUserID] = next
	return nil
}

type fakeShops struct {
	shops []bot.AccountShop
	err   error
}

func (f *fakeShops) ShopsForUser(ctx context.Context, discordUserID, language string) ([]bot.AccountShop, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.shops, nil
}

type memLang struct {
	byUser map[string]string
}

func (m *memLang) GetUserLanguage(discordUserID string) (string, error) {
	if m.byUser == nil {
		return "ko", nil
	}
	if lang, ok := m.byUser[discordUserID]; ok {
		return lang, nil
	}
	return "ko", nil
}

func (m *memLang) SetUserLanguage(discordUserID, language string) error {
	if m.byUser == nil {
		m.byUser = map[string]string{}
	}
	m.byUser[discordUserID] = language
	return nil
}

func TestHandleAuth_EphemeralLoginURL(t *testing.T) {
	h := &bot.Handlers{
		Auth: &fakeAuth{url: "https://auth.example/login", state: "st"},
	}
	resp, err := h.HandleAuth("discord-1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Ephemeral {
		t.Error("expected ephemeral")
	}
	if resp.Content == "" {
		t.Fatal("expected content")
	}
	if len(resp.Components) == 0 {
		t.Fatal("expected login button component")
	}
	row, ok := resp.Components[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("component type %T", resp.Components[0])
	}
	btn, ok := row.Components[0].(discordgo.Button)
	if !ok || btn.URL != "https://auth.example/login" {
		t.Fatalf("button = %#v", row.Components[0])
	}
}

func TestHandleAuth_Error(t *testing.T) {
	h := &bot.Handlers{
		Auth: &fakeAuth{err: errors.New("boom")},
	}
	_, err := h.HandleAuth("u", i18n.KO)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHandleAccounts_List(t *testing.T) {
	h := &bot.Handlers{
		Accounts: &memAccounts{byUser: map[string][]store.Account{
			"u1": {
				{GameName: "Ace", TagLine: "KR1", Region: "kr", PUUID: "p1"},
				{GameName: "Beta", TagLine: "NA1", Region: "na", PUUID: "p2"},
			},
		}},
	}
	resp, err := h.HandleAccounts("u1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Ephemeral {
		t.Error("expected ephemeral")
	}
	if !strings.Contains(resp.Content, "Ace#KR1 (kr)") {
		t.Fatalf("content %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "Beta#NA1 (na)") {
		t.Fatalf("content %q", resp.Content)
	}
}

func TestHandleAccounts_Empty(t *testing.T) {
	h := &bot.Handlers{Accounts: &memAccounts{byUser: map[string][]store.Account{}}}
	resp, err := h.HandleAccounts("nobody", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "연결된 계정이 없습니다") {
		t.Fatalf("content %q", resp.Content)
	}
}

func TestHandleUnlink_ByPUUID(t *testing.T) {
	accts := &memAccounts{byUser: map[string][]store.Account{
		"u1": {{PUUID: "puuid-del", GameName: "X", TagLine: "Y", Region: "na"}},
	}}
	h := &bot.Handlers{Accounts: accts}
	resp, err := h.HandleUnlink("u1", "puuid-del", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "연결 해제") {
		t.Fatalf("content %q", resp.Content)
	}
	list, _ := accts.ListRiotAccountsByDiscord("u1")
	if len(list) != 0 {
		t.Fatalf("still have accounts: %+v", list)
	}
}

func TestHandleUnlink_ByGameName(t *testing.T) {
	accts := &memAccounts{byUser: map[string][]store.Account{
		"u1": {{PUUID: "puuid-1", GameName: "MyName", TagLine: "TAG", Region: "eu"}},
	}}
	h := &bot.Handlers{Accounts: accts}
	_, err := h.HandleUnlink("u1", "MyName", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := accts.ListRiotAccountsByDiscord("u1")
	if len(list) != 0 {
		t.Fatal("expected deleted")
	}
}

func TestHandleUnlink_NotFound(t *testing.T) {
	h := &bot.Handlers{Accounts: &memAccounts{byUser: map[string][]store.Account{}}}
	_, err := h.HandleUnlink("u1", "missing", i18n.KO)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHandleShop_NoAccounts(t *testing.T) {
	h := &bot.Handlers{
		Accounts: &memAccounts{byUser: map[string][]store.Account{}},
		Shops:    &fakeShops{},
	}
	resp, err := h.HandleShop(context.Background(), "u1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Ephemeral {
		t.Error("expected ephemeral")
	}
	if !strings.Contains(resp.Content, "인증된 계정이 없습니다") {
		t.Fatalf("content %q", resp.Content)
	}
}

func TestHandleShop_Embeds(t *testing.T) {
	h := &bot.Handlers{
		Accounts: &memAccounts{byUser: map[string][]store.Account{
			"u1": {{PUUID: "p1"}},
		}},
		Shops: &fakeShops{shops: []bot.AccountShop{
			{
				GameName: "Player",
				TagLine:  "KR1",
				Offers: []bot.OfferView{
					{DisplayName: "Prime Vandal", CostVP: 1775, IconURL: "https://icon"},
				},
			},
		}},
	}
	resp, err := h.HandleShop(context.Background(), "u1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Embeds) != 1 {
		t.Fatalf("embeds %d", len(resp.Embeds))
	}
	if !strings.Contains(resp.Embeds[0].Title, "Prime Vandal") {
		t.Fatalf("title %q", resp.Embeds[0].Title)
	}
	if !strings.Contains(resp.Embeds[0].Title, "1775") {
		t.Fatalf("title should include VP: %q", resp.Embeds[0].Title)
	}
	if resp.Embeds[0].Author == nil || resp.Embeds[0].Author.Name != "Player#KR1" {
		t.Fatalf("author %+v", resp.Embeds[0].Author)
	}
	if resp.Embeds[0].Thumbnail == nil || resp.Embeds[0].Thumbnail.URL != "https://icon" {
		t.Fatalf("thumbnail %+v", resp.Embeds[0].Thumbnail)
	}
	if resp.Embeds[0].Image != nil {
		t.Fatal("expected no full-size Image (use Thumbnail for compact shop)")
	}
}

func TestHandleLanguage_SetEnglish(t *testing.T) {
	langStore := &memLang{byUser: map[string]string{}}
	h := &bot.Handlers{Lang: langStore}
	resp, err := h.HandleLanguage("u1", "en")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "English") {
		t.Fatalf("content %q", resp.Content)
	}
	if langStore.byUser["u1"] != "en" {
		t.Fatalf("stored %q", langStore.byUser["u1"])
	}
}

func TestCommands_Names(t *testing.T) {
	names := map[string]bool{}
	for _, c := range bot.Commands() {
		names[c.Name] = true
	}
	for _, want := range []string{"auth", "accounts", "unlink", "shop", "wishlist", "channel", "language"} {
		if !names[want] {
			t.Fatalf("missing command %q, got %v", want, names)
		}
	}
}
