package bot_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	url        string
	state      string
	err        error
	display    string
	waitErr    error
	pwDisplay  string
	pwMFA      string
	pwHint     string
	pwErr      error
	mfaDisplay string
	mfaErr     error
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

func (f *fakeAuth) LoginWithPassword(ctx context.Context, discordUserID, username, password string) (string, string, string, error) {
	if f.pwErr != nil {
		return "", "", "", f.pwErr
	}
	return f.pwDisplay, f.pwMFA, f.pwHint, nil
}

func (f *fakeAuth) CompletePasswordMFA(ctx context.Context, mfaState, code string) (string, error) {
	if f.mfaErr != nil {
		return "", f.mfaErr
	}
	return f.mfaDisplay, nil
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

func TestHandleAuth_Chooser(t *testing.T) {
	h := &bot.Handlers{Auth: &fakeAuth{}}
	resp, err := h.HandleAuth(context.Background(), "u1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "연동 방식") {
		t.Fatalf("content %q", resp.Content)
	}
	row := resp.Components[0].(discordgo.ActionsRow)
	if len(row.Components) != 2 {
		t.Fatalf("buttons %#v", row.Components)
	}
}

func TestHandleAuthQR_QRCodeAttachment(t *testing.T) {
	const scanURL = "https://qrlogin.riotgames.com/riotmobile?suuid=s1"
	h := &bot.Handlers{
		Auth: &fakeAuth{url: scanURL, state: "st"},
	}
	resp, state, err := h.HandleAuthQR(context.Background(), "discord-1", i18n.KO)
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

	row, ok := resp.Components[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("component type %T", resp.Components[0])
	}
	btn, ok := row.Components[0].(discordgo.Button)
	if !ok || btn.URL != scanURL {
		t.Fatalf("button = %#v", row.Components[0])
	}
}

func TestHandleAuthQR_NoLocalhostCatcherDependency(t *testing.T) {
	h := &bot.Handlers{Auth: &fakeAuth{url: "https://qrlogin.riotgames.com/riotmobile?suuid=s1", state: "st"}}
	resp, _, err := h.HandleAuthQR(context.Background(), "u", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"localhost", "127.0.0.1", "catcher", "AUTH_BASE_URL", "포트 80"} {
		if strings.Contains(resp.Content, bad) {
			t.Fatalf("QR auth prompt must not mention %q: %q", bad, resp.Content)
		}
	}
}

func TestHandleAuthQR_Error(t *testing.T) {
	h := &bot.Handlers{
		Auth: &fakeAuth{err: errors.New("boom")},
	}
	_, _, err := h.HandleAuthQR(context.Background(), "u", i18n.KO)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHandlePasswordLogin_Success(t *testing.T) {
	h := &bot.Handlers{Auth: &fakeAuth{pwDisplay: "Ace#KR1"}}
	resp, err := h.HandlePasswordLogin(context.Background(), "u1", "user", "pass", "", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "Ace#KR1") {
		t.Fatalf("content %q", resp.Content)
	}
}

func TestHandlePasswordLogin_MFAButton(t *testing.T) {
	h := &bot.Handlers{Auth: &fakeAuth{pwMFA: "mfa-1", pwHint: "a***@ex.com"}}
	resp, err := h.HandlePasswordLogin(context.Background(), "u1", "user", "pass", "", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "a***@ex.com") {
		t.Fatalf("content %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "Riot Mobile") {
		t.Fatalf("expected Riot Mobile hint in %q", resp.Content)
	}
	row := resp.Components[0].(discordgo.ActionsRow)
	btn := row.Components[0].(discordgo.Button)
	if btn.CustomID != "auth:mfaopen:mfa-1" {
		t.Fatalf("customID %q", btn.CustomID)
	}
}

func TestHandlePasswordLogin_MFACodeInline(t *testing.T) {
	h := &bot.Handlers{Auth: &fakeAuth{pwMFA: "mfa-1", mfaDisplay: "Ace#KR1"}}
	resp, err := h.HandlePasswordLogin(context.Background(), "u1", "user", "pass", "123456", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "Ace#KR1") {
		t.Fatalf("content %q", resp.Content)
	}
}

func TestHandlePasswordLogin_MFAAuthenticatorPrompt(t *testing.T) {
	h := &bot.Handlers{Auth: &fakeAuth{pwMFA: "mfa-1", pwHint: "authenticator"}}
	resp, err := h.HandlePasswordLogin(context.Background(), "u1", "user", "pass", "", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "Riot Mobile") {
		t.Fatalf("content %q", resp.Content)
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
					{DisplayName: "Reaver Sheriff", CostVP: 1775, IconURL: "https://icon2"},
					{DisplayName: "Ion Phantom", CostVP: 1775, IconURL: "https://icon3"},
					{DisplayName: "Oni Operator", CostVP: 2475, IconURL: "https://icon4"},
				},
			},
		}},
	}
	resp, err := h.HandleShop(context.Background(), "u1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "Player#KR1") {
		t.Fatalf("page content should include account: %q", resp.Content)
	}
	if len(resp.Components) != 0 {
		t.Fatalf("single account should have no nav buttons, got %d", len(resp.Components))
	}
	if len(resp.Embeds) != 4 {
		t.Fatalf("expected 4 skin embeds on one account page, got %d", len(resp.Embeds))
	}
	if resp.Embeds[0].Author != nil {
		t.Fatalf("account should be in message content, not embed author: %+v", resp.Embeds[0].Author)
	}
	if !strings.Contains(resp.Embeds[0].Title, "Prime Vandal") {
		t.Fatalf("title %q", resp.Embeds[0].Title)
	}
	if !strings.Contains(resp.Embeds[0].Title, "1775") {
		t.Fatalf("title should include VP: %q", resp.Embeds[0].Title)
	}
	if resp.Embeds[0].Thumbnail == nil || resp.Embeds[0].Thumbnail.URL != "https://icon" {
		t.Fatalf("thumbnail %+v", resp.Embeds[0].Thumbnail)
	}
	if resp.Embeds[3].Thumbnail == nil || resp.Embeds[3].Thumbnail.URL != "https://icon4" {
		t.Fatalf("4th skin thumbnail %+v", resp.Embeds[3].Thumbnail)
	}
	for i, e := range resp.Embeds {
		if e.Image != nil {
			t.Fatalf("embed %d: expected Thumbnail not full-size Image", i)
		}
	}
}

func TestBuildAccountPageEmbeds_RarityColors(t *testing.T) {
	embeds := bot.BuildAccountPageEmbeds(bot.AccountShop{
		GameName: "P",
		TagLine:  "1",
		Offers: []bot.OfferView{
			{DisplayName: "Select", CostVP: 875, Color: 0x5a9fe2},
			{DisplayName: "Exclusive", CostVP: 2175, Color: 0xf5955b},
			{DisplayName: "Unknown", CostVP: 1275},
		},
	}, i18n.KO)
	if len(embeds) != 3 {
		t.Fatalf("got %d embeds", len(embeds))
	}
	if embeds[0].Color != 0x5a9fe2 {
		t.Fatalf("select color %#x", embeds[0].Color)
	}
	if embeds[1].Color != 0xf5955b {
		t.Fatalf("exclusive color %#x", embeds[1].Color)
	}
	if embeds[2].Color != 0xFD4553 {
		t.Fatalf("fallback color %#x", embeds[2].Color)
	}
}

func TestHandleShop_MultiAccountHasNavButtons(t *testing.T) {
	h := &bot.Handlers{
		Accounts: &memAccounts{byUser: map[string][]store.Account{
			"u1": {{PUUID: "p1"}, {PUUID: "p2"}},
		}},
		Shops: &fakeShops{shops: []bot.AccountShop{
			{GameName: "A", TagLine: "1", Offers: []bot.OfferView{{DisplayName: "SkinA", CostVP: 1}}},
			{GameName: "B", TagLine: "2", Offers: []bot.OfferView{{DisplayName: "SkinB", CostVP: 2}}},
		}},
	}
	resp, err := h.HandleShop(context.Background(), "u1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "A#1") || !strings.Contains(resp.Content, "1 / 2") {
		t.Fatalf("content %q", resp.Content)
	}
	if len(resp.Embeds) != 1 {
		t.Fatalf("should show one account page; with 1 skin got embeds=%d", len(resp.Embeds))
	}
	if resp.Embeds[0].Author != nil {
		t.Fatalf("account should be in message content, not embed author: %+v", resp.Embeds[0].Author)
	}
	if len(resp.Components) != 1 {
		t.Fatalf("expected nav row, got %d", len(resp.Components))
	}
	row, ok := resp.Components[0].(discordgo.ActionsRow)
	if !ok || len(row.Components) != 3 {
		t.Fatalf("nav row %#v", resp.Components[0])
	}
	prev, _ := row.Components[0].(discordgo.Button)
	next, _ := row.Components[2].(discordgo.Button)
	if !prev.Disabled {
		t.Fatal("prev should be disabled on first page")
	}
	if next.Disabled {
		t.Fatal("next should be enabled on first page")
	}
	if !strings.Contains(next.CustomID, "shop:page:u1:1") {
		t.Fatalf("next customID %q", next.CustomID)
	}

	page2, err := h.HandleShopNav("u1", 1, "u1", i18n.KO)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page2.Content, "B#2") || !strings.Contains(page2.Content, "2 / 2") {
		t.Fatalf("page2 content %q", page2.Content)
	}
	if page2.Embeds[0].Author != nil {
		t.Fatalf("account should be in message content, not embed author: %+v", page2.Embeds[0].Author)
	}
	row2 := page2.Components[0].(discordgo.ActionsRow)
	prev2 := row2.Components[0].(discordgo.Button)
	next2 := row2.Components[2].(discordgo.Button)
	if prev2.Disabled {
		t.Fatal("prev should be enabled on last page")
	}
	if !next2.Disabled {
		t.Fatal("next should be disabled on last page")
	}

	if denied, err := h.HandleShopNav("u1", 1, "other", i18n.KO); err != nil {
		t.Fatal(err)
	} else {
		if !denied.Ephemeral {
			t.Fatal("denial must be ephemeral (only the clicker sees it)")
		}
		if len(denied.Embeds) != 1 || !strings.Contains(denied.Embeds[0].Description, "본인만") {
			t.Fatalf("expected denial embed, got %+v", denied)
		}
	}
}

// TestBuildShopEmbeds_OneEmbedPerAccount guards against Discord's 10-embed-
// per-message limit: 3 accounts x 4 skins used to produce 12 embeds (one per
// skin) and silently fail the deferred interaction edit.
func TestBuildShopEmbeds_OneEmbedPerAccount(t *testing.T) {
	shops := make([]bot.AccountShop, 0, 3)
	for i := 0; i < 3; i++ {
		offers := make([]bot.OfferView, 0, 4)
		for j := 0; j < 4; j++ {
			offers = append(offers, bot.OfferView{
				DisplayName: fmt.Sprintf("Skin%d-%d", i, j),
				CostVP:      1000 + j,
			})
		}
		shops = append(shops, bot.AccountShop{
			GameName: fmt.Sprintf("Player%d", i),
			TagLine:  "KR1",
			Offers:   offers,
		})
	}

	embeds := bot.BuildShopEmbeds(shops, i18n.KO)
	if len(embeds) != 3 {
		t.Fatalf("expected exactly 3 embeds (1 per account), got %d", len(embeds))
	}
	if len(embeds) > 10 {
		t.Fatalf("embeds exceed Discord's 10-embed-per-message limit: %d", len(embeds))
	}
	for i, e := range embeds {
		if len(e.Fields) != 4 {
			t.Fatalf("embed %d: expected 4 skin fields, got %d", i, len(e.Fields))
		}
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
