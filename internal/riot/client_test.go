package riot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(baseURL string, srv *httptest.Server) *Client {
	c := NewClient(srv.Client())
	if baseURL != "" {
		c.AuthBaseURL = baseURL
		c.EntitlementsBaseURL = baseURL
		c.PDBaseURLFunc = func(shard string) string { return baseURL }
	}
	return c
}

func TestParseRedirectURL_PlayValorantKO(t *testing.T) {
	raw := "https://playvalorant.com/ko-kr/opt_in/#access_token=acc123&scope=account+openid&id_token=id456&token_type=Bearer&expires_in=3600"
	acc, id, err := ParseRedirectURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	if acc != "acc123" || id != "id456" {
		t.Fatalf("got %q %q", acc, id)
	}
}

func TestParseRedirectURL_Fragment(t *testing.T) {
	raw := "http://localhost/redirect#access_token=acc123&id_token=id456&expires_in=3600"
	acc, id, err := ParseRedirectURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	if acc != "acc123" || id != "id456" {
		t.Fatalf("got acc=%q id=%q", acc, id)
	}
}

func TestParseRedirectURL_Query(t *testing.T) {
	raw := "http://localhost/redirect?access_token=accQ&id_token=idQ"
	acc, id, err := ParseRedirectURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	if acc != "accQ" || id != "idQ" {
		t.Fatalf("got acc=%q id=%q", acc, id)
	}
}

func TestParseRedirectURL_MissingTokens(t *testing.T) {
	_, _, err := ParseRedirectURL("http://localhost/redirect#expires_in=3600")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGetEntitlements(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/token/v1" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-xyz" {
			t.Errorf("Authorization: %q", got)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type: %q", ct)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"entitlements_token": "ent-jwt"})
	}))
	defer srv.Close()

	c := testClient(srv.URL, srv)
	tok, err := c.GetEntitlements(context.Background(), "access-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ent-jwt" {
		t.Fatalf("got %q", tok)
	}
}

func TestGetUserInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/userinfo" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": "puuid-abc"})
	}))
	defer srv.Close()

	c := testClient(srv.URL, srv)
	puuid, err := c.GetUserInfo(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if puuid != "puuid-abc" {
		t.Fatalf("got %q", puuid)
	}
}

func TestGetPlayerNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/name-service/v2/players") {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		var body []string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 1 || body[0] != "puuid-abc" {
			t.Fatalf("body %v", body)
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"Subject": "puuid-abc", "GameName": "Player", "TagLine": "NA1"},
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, srv)
	names, err := c.GetPlayerNames(context.Background(), "a", "e", "na", []string{"puuid-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0].GameName != "Player" || names[0].TagLine != "NA1" {
		t.Fatalf("%+v", names)
	}
}

func TestGetStorefront(t *testing.T) {
	skinUUID := "skin-uuid-1"
	vpCurrency := "85ad13f7-3d1b-5128-9eb2-7cd8ee0b5741"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/store/v3/storefront/puuid-1") {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer acc" {
			t.Errorf("auth")
		}
		if r.Header.Get("X-Riot-Entitlements-JWT") != "ent" {
			t.Errorf("entitlements header")
		}
		if r.Header.Get("X-Riot-ClientPlatform") != DefaultClientPlatform {
			t.Errorf("platform %q", r.Header.Get("X-Riot-ClientPlatform"))
		}
		if r.Header.Get("X-Riot-ClientVersion") != DefaultClientVersion {
			t.Errorf("version %q", r.Header.Get("X-Riot-ClientVersion"))
		}

		resp := map[string]any{
			"SkinsPanelLayout": map[string]any{
				"SingleItemStoreOffers": []map[string]any{
					{
						"OfferID": skinUUID,
						"Cost": map[string]int{
							vpCurrency: 1775,
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := testClient(srv.URL, srv)
	sf, err := c.GetStorefront(context.Background(), "acc", "ent", "na", "puuid-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Offers) != 1 {
		t.Fatalf("offers %d", len(sf.Offers))
	}
	if sf.Offers[0].SkinUUID != skinUUID || sf.Offers[0].CostVP != 1775 {
		t.Fatalf("%+v", sf.Offers[0])
	}
}

func TestGetStorefront_SingleItemOffersFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Real API: SingleItemOffers is []string (UUIDs), not offer objects.
		resp := map[string]any{
			"SkinsPanelLayout": map[string]any{
				"SingleItemOffers": []string{
					"8e032580-414f-7ab4-7efb-f6ac7d2bc2f1",
					"9ff1a0e6-4f74-0e6d-9117-de8683db4eb5",
				},
				"SingleItemOffersRemainingDurationInSeconds": 3732,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := testClient(srv.URL, srv)
	sf, err := c.GetStorefront(context.Background(), "a", "e", "eu", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Offers) != 2 {
		t.Fatalf("offers %+v", sf.Offers)
	}
	if sf.Offers[0].SkinUUID != "8e032580-414f-7ab4-7efb-f6ac7d2bc2f1" {
		t.Fatalf("%+v", sf.Offers[0])
	}
	if sf.RemainsUntilReset != 3732*time.Second {
		t.Fatalf("reset %v", sf.RemainsUntilReset)
	}
}

func TestGetStorefront_RealShapeWithBothFields(t *testing.T) {
	vp := VPCurrencyUUID
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"SkinsPanelLayout": map[string]any{
				"SingleItemOffers": []string{"uuid-a", "uuid-b"},
				"SingleItemStoreOffers": []map[string]any{
					{"OfferID": "uuid-a", "Cost": map[string]int{vp: 1775}},
					{"OfferID": "uuid-b", "Cost": map[string]int{vp: 875}},
				},
				"SingleItemOffersRemainingDurationInSeconds": 100,
			},
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, srv)
	sf, err := c.GetStorefront(context.Background(), "a", "e", "kr", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Offers) != 2 || sf.Offers[0].CostVP != 1775 || sf.Offers[1].CostVP != 875 {
		t.Fatalf("%+v", sf.Offers)
	}
}

func TestCookieReauth(t *testing.T) {
	redirectWithTokens := "https://playvalorant.com/opt_in#access_token=cookie-acc&id_token=cookie-id"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/authorize" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Cookie") != "ssid=test-cookie" {
			t.Errorf("cookie %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Location", redirectWithTokens)
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := NewClient(srv.Client())
	c.AuthBaseURL = srv.URL
	acc, id, err := c.CookieReauth(context.Background(), "ssid=test-cookie")
	if err != nil {
		t.Fatal(err)
	}
	if acc != "cookie-acc" || id != "cookie-id" {
		t.Fatalf("acc=%q id=%q", acc, id)
	}
}

func TestRegionFromToken(t *testing.T) {
	payload := base64URLEncodeJSON(map[string]any{
		"dat": map[string]string{"r": "na"},
	})
	token := "hdr." + payload + ".sig"

	region, shard, err := RegionFromToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if region != "na" || shard != "na" {
		t.Fatalf("region=%q shard=%q", region, shard)
	}
}

func TestRegionFromToken_MissingRegion(t *testing.T) {
	payload := base64URLEncodeJSON(map[string]string{"sub": "x"})
	token := "hdr." + payload + ".sig"
	_, _, err := RegionFromToken(token)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestShardForRegion(t *testing.T) {
	cases := map[string]string{
		"na": "na", "latam": "na", "br": "na",
		"eu": "eu", "ap": "ap", "kr": "kr",
	}
	for region, want := range cases {
		got, err := ShardForRegion(region)
		if err != nil || got != want {
			t.Errorf("ShardForRegion(%q) = %q, %v", region, got, err)
		}
	}
}

func TestShardForRegion_Unknown(t *testing.T) {
	_, err := ShardForRegion("mars")
	if err == nil {
		t.Fatal("expected error")
	}
}

func base64URLEncodeJSON(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}
