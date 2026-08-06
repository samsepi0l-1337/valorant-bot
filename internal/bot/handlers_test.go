package bot_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/png"
	"io"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/dosfsociety/valorant-bot/internal/bot"
	"github.com/dosfsociety/valorant-bot/internal/i18n"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

type fakeAuth struct {
	url     string
	state   string
	err     error
	display string
	waitErr error
}

func (f *fakeAuth) BeginQRAuth(ctx context.Context, discordUserID string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.url, f.state, nil
}

func (f *fakeAuth) WaitQRLogin(ctx context.Context, state string) (string, error) {
	if f.waitErr != nil {
		return "", f.waitErr
	}
	return f.display, nil
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

func TestHandleAuth_QRCodeAttachment(t *testing.T) {
	const scanURL = "https://qrlogin.riotgames.com/riotmobile?suuid=s1"
	h := &bot.Handlers{
		Auth: &fakeAuth{url: scanURL, state: "st"},
	}
	resp, state, err := h.HandleAuth(context.Background(), "discord-1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if state != "st" {
		t.Fatalf("state = %q", state)
	}
	if !resp.Ephemeral {
		t.Error("expected ephemeral")
	}
	if !strings.Contains(resp.Content, "Riot Mobile") {
		t.Fatalf("content should tell the user to use Riot Mobile: %q", resp.Content)
	}

	if len(resp.Files) != 1 {
		t.Fatalf("expected one QR attachment, got %d", len(resp.Files))
	}
	png, err := io.ReadAll(resp.Files[0].Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := image.Decode(bytes.NewReader(png)); err != nil {
		t.Fatalf("attachment is not a decodable image: %v", err)
	}
	if len(resp.Embeds) != 1 || resp.Embeds[0].Image == nil {
		t.Fatalf("expected an embed showing the QR image: %+v", resp.Embeds)
	}
	if want := "attachment://" + resp.Files[0].Name; resp.Embeds[0].Image.URL != want {
		t.Fatalf("embed image URL = %q, want %q", resp.Embeds[0].Image.URL, want)
	}

	// Mobile users tap the link instead of scanning.
	row, ok := resp.Components[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("component type %T", resp.Components[0])
	}
	btn, ok := row.Components[0].(discordgo.Button)
	if !ok || btn.URL != scanURL {
		t.Fatalf("button = %#v", row.Components[0])
	}
}

func TestHandleAuth_NoLocalhostCatcherDependency(t *testing.T) {
	h := &bot.Handlers{Auth: &fakeAuth{url: "https://qrlogin.riotgames.com/riotmobile?suuid=s1", state: "st"}}
	resp, _, err := h.HandleAuth(context.Background(), "u", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"localhost", "127.0.0.1", "catcher", "AUTH_BASE_URL", "포트 80"} {
		if strings.Contains(resp.Content, bad) {
			t.Fatalf("QR auth prompt must not mention %q: %q", bad, resp.Content)
		}
	}
}

func TestHandleAuth_Error(t *testing.T) {
	h := &bot.Handlers{
		Auth: &fakeAuth{err: errors.New("boom")},
	}
	_, _, err := h.HandleAuth(context.Background(), "u", i18n.KO)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHandleAuthComplete(t *testing.T) {
	h := &bot.Handlers{}

	done := h.HandleAuthComplete("Ace#KR1", nil, i18n.KO)
	if !strings.Contains(done.Content, "Ace#KR1") || !done.Ephemeral {
		t.Fatalf("done = %+v", done)
	}
	if done.Embeds == nil || done.Components == nil {
		t.Fatal("completion must clear the QR embed and button")
	}
	if len(done.Embeds) != 0 || len(done.Components) != 0 {
		t.Fatalf("expected cleared embeds/components: %+v", done)
	}

	timedOut := h.HandleAuthComplete("", context.DeadlineExceeded, i18n.KO)
	if !strings.Contains(timedOut.Content, "/auth") {
		t.Fatalf("timeout message should ask the user to retry: %q", timedOut.Content)
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
	if !strings.Contains(resp.Content, "Ace#KR1 (한국 (kr))") {
		t.Fatalf("content %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "Beta#NA1 (북미 (na))") {
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
