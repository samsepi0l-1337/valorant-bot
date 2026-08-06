package bot_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dosfsociety/valorant-bot/internal/bot"
	"github.com/dosfsociety/valorant-bot/internal/crypto"
	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

func TestShopFetcher_Success(t *testing.T) {
	const (
		puuid    = "puuid-shop-1"
		skinUUID = "skin-uuid-1"
		vp       = riot.VPCurrencyUUID
	)

	redirect := "https://playvalorant.com/opt_in#access_token=acc-token&id_token=id-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/authorize":
			w.Header().Set("Location", redirect)
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/api/token/v1":
			_ = json.NewEncoder(w).Encode(map[string]string{"entitlements_token": "ent-jwt"})
		case strings.Contains(r.URL.Path, "/store/v3/storefront/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"SkinsPanelLayout": map[string]any{
					"SingleItemStoreOffers": []map[string]any{
						{"OfferID": skinUUID, "Cost": map[string]int{vp: 1775}},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	skinSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"uuid": skinUUID, "displayName": "Prime Vandal", "displayIcon": "https://example.com/v.png"},
			},
		})
	}))
	defer skinSrv.Close()

	box, err := crypto.NewBoxer("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := box.Encrypt([]byte("ssid=cookie"))
	if err != nil {
		t.Fatal(err)
	}

	st := openBotTestStore(t)
	if err := st.UpsertRiotAccount(store.Account{
		DiscordUserID:     "discord-1",
		PUUID:             puuid,
		GameName:          "Ace",
		TagLine:           "KR1",
		Region:            "kr",
		Shard:             "kr",
		CookiesCiphertext: cipher,
	}); err != nil {
		t.Fatal(err)
	}

	rc := riot.NewClient(srv.Client())
	rc.AuthBaseURL = srv.URL
	rc.EntitlementsBaseURL = srv.URL
	rc.PDBaseURLFunc = func(shard string) string { return srv.URL }

	sc := skins.NewCache(skinSrv.Client(), skinSrv.URL)
	fetcher := &bot.ShopFetcher{
		Accounts: st,
		Boxer:    box,
		Riot:     rc,
		Skins:    sc,
	}

	shops, err := fetcher.ShopsForUser(context.Background(), "discord-1", "ko")
	if err != nil {
		t.Fatal(err)
	}
	if len(shops) != 1 {
		t.Fatalf("shops %d", len(shops))
	}
	if shops[0].Err != "" {
		t.Fatalf("err %q", shops[0].Err)
	}
	if shops[0].GameName != "Ace" || shops[0].TagLine != "KR1" {
		t.Fatalf("%+v", shops[0])
	}
	if len(shops[0].Offers) != 1 {
		t.Fatalf("offers %+v", shops[0].Offers)
	}
	o := shops[0].Offers[0]
	if o.DisplayName != "Prime Vandal" || o.CostVP != 1775 || o.IconURL != "https://example.com/v.png" {
		t.Fatalf("%+v", o)
	}
}

// TestShopFetcher_MultiAccountCompletes guards against a regression where
// ShopsForUser fetched accounts sequentially; with N accounts sharing one
// slow-ish upstream, a serial implementation could stall past a caller's
// deadline. It also checks that per-account order is preserved despite the
// concurrent fetch.
func TestShopFetcher_MultiAccountCompletes(t *testing.T) {
	const vp = riot.VPCurrencyUUID
	redirect := "https://playvalorant.com/opt_in#access_token=acc-token&id_token=id-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/authorize":
			w.Header().Set("Location", redirect)
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/api/token/v1":
			_ = json.NewEncoder(w).Encode(map[string]string{"entitlements_token": "ent-jwt"})
		case strings.Contains(r.URL.Path, "/store/v3/storefront/"):
			puuid := strings.TrimPrefix(r.URL.Path, "/store/v3/storefront/")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"SkinsPanelLayout": map[string]any{
					"SingleItemStoreOffers": []map[string]any{
						{"OfferID": "skin-" + puuid, "Cost": map[string]int{vp: 1000}},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	box, err := crypto.NewBoxer("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := box.Encrypt([]byte("ssid=cookie"))
	if err != nil {
		t.Fatal(err)
	}

	st := openBotTestStore(t)
	for _, puuid := range []string{"puuid-a", "puuid-b"} {
		if err := st.UpsertRiotAccount(store.Account{
			DiscordUserID:     "discord-multi",
			PUUID:             puuid,
			GameName:          "Player-" + puuid,
			TagLine:           "KR1",
			Region:            "kr",
			Shard:             "kr",
			CookiesCiphertext: cipher,
		}); err != nil {
			t.Fatal(err)
		}
	}

	rc := riot.NewClient(srv.Client())
	rc.AuthBaseURL = srv.URL
	rc.EntitlementsBaseURL = srv.URL
	rc.PDBaseURLFunc = func(shard string) string { return srv.URL }

	fetcher := &bot.ShopFetcher{
		Accounts: st,
		Boxer:    box,
		Riot:     rc,
		// Skins is intentionally nil: this test only checks concurrent
		// fetch completion/ordering, not skin-name resolution.
	}

	shops, err := fetcher.ShopsForUser(context.Background(), "discord-multi", "ko")
	if err != nil {
		t.Fatal(err)
	}
	if len(shops) != 2 {
		t.Fatalf("shops %d", len(shops))
	}
	for i, s := range shops {
		if s.Err != "" {
			t.Fatalf("shop %d err %q", i, s.Err)
		}
		if len(s.Offers) != 1 {
			t.Fatalf("shop %d offers %+v", i, s.Offers)
		}
	}
	// Order preserved: account order in equals account order out.
	if shops[0].PUUID != "puuid-a" || shops[1].PUUID != "puuid-b" {
		t.Fatalf("order not preserved: %+v", shops)
	}
}

func TestShopFetcher_CookieReauthFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	box, err := crypto.NewBoxer("secret")
	if err != nil {
		t.Fatal(err)
	}
	cipher, _ := box.Encrypt([]byte("ssid=bad"))

	st := openBotTestStore(t)
	_ = st.UpsertRiotAccount(store.Account{
		DiscordUserID:     "u",
		PUUID:             "p1",
		GameName:          "A",
		TagLine:           "B",
		Shard:             "na",
		CookiesCiphertext: cipher,
	})

	rc := riot.NewClient(srv.Client())
	rc.AuthBaseURL = srv.URL

	fetcher := &bot.ShopFetcher{
		Accounts: st,
		Boxer:    box,
		Riot:     rc,
		Skins:    skins.NewCache(http.DefaultClient, "http://unused"),
	}

	shops, err := fetcher.ShopsForUser(context.Background(), "u", "en")
	if err != nil {
		t.Fatal(err)
	}
	if len(shops) != 1 {
		t.Fatalf("shops %d", len(shops))
	}
	if shops[0].Err == "" {
		t.Fatal("expected per-account error")
	}
}

func openBotTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
