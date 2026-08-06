package riot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeRegion(t *testing.T) {
	cases := map[string]string{
		"ap": "ap", "AP": "ap", "ap1": "ap", "asia": "ap", "apac": "ap",
		"kr": "kr", "KR": "kr", "kr1": "kr",
		"na": "na", "na1": "na",
		"latam": "latam", "br": "br",
		"eu": "eu", "euw": "eu", "euw1": "eu", "eun": "eu", "eun1": "eu",
		"": "", "mars": "",
	}
	for in, want := range cases {
		if got := NormalizeRegion(in); got != want {
			t.Errorf("NormalizeRegion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShardForRegion_Aliases(t *testing.T) {
	cases := map[string]string{
		"ap": "ap", "asia": "ap", "ap1": "ap",
		"kr": "kr", "kr1": "kr",
		"na": "na", "latam": "na", "br": "na",
		"eu": "eu", "euw1": "eu",
	}
	for region, want := range cases {
		got, err := ShardForRegion(region)
		if err != nil || got != want {
			t.Errorf("ShardForRegion(%q) = %q, %v want %q", region, got, err, want)
		}
	}
}

func TestRegionFromToken_PlatformCodes(t *testing.T) {
	// Riot often puts platform-style codes (AP1/KR1) in dat.r.
	payload := base64URLEncodeJSON(map[string]any{
		"dat": map[string]string{"r": "AP1"},
	})
	token := "hdr." + payload + ".sig"
	region, shard, err := RegionFromToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if region != "ap" || shard != "ap" {
		t.Fatalf("region=%q shard=%q", region, shard)
	}
}

func TestDisplayRegion(t *testing.T) {
	if got := DisplayRegion("ap", "ko"); got != "아시아 (ap)" {
		t.Fatalf("ko ap = %q", got)
	}
	if got := DisplayRegion("kr", "en"); got != "Korea (kr)" {
		t.Fatalf("en kr = %q", got)
	}
	if got := DisplayRegion("ap", ""); got != "Asia Pacific (ap)" {
		t.Fatalf("default ap = %q", got)
	}
}

func TestClient_GetValorantAffinity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/pas/v1/product/valorant" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-tok" {
			t.Fatalf("auth %q", got)
		}
		var body struct {
			IDToken string `json:"id_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.IDToken != "id-tok" {
			t.Fatalf("id_token %q", body.IDToken)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"affinities": map[string]string{"live": "ap", "pbe": "na"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.Client())
	c.GeoBaseURL = srv.URL
	got, err := c.GetValorantAffinity(context.Background(), "access-tok", "id-tok")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ap" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveValorantRegion_PrefersGeo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"affinities": map[string]string{"live": "ap"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.Client())
	c.GeoBaseURL = srv.URL

	// Token claims KR, but Riot Geo says AP — Geo wins for Valorant.
	tokenPayload := base64URLEncodeJSON(map[string]any{
		"dat": map[string]string{"r": "kr"},
	})
	access := "hdr." + tokenPayload + ".sig"
	region, shard, err := c.ResolveValorantRegion(context.Background(), access, "id-tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if region != "ap" || shard != "ap" {
		t.Fatalf("region=%q shard=%q", region, shard)
	}
}

func TestResolveValorantRegion_TokenWhenNoIDToken(t *testing.T) {
	c := NewClient(nil)
	tokenPayload := base64URLEncodeJSON(map[string]any{
		"dat": map[string]string{"r": "AP1"},
	})
	access := "hdr." + tokenPayload + ".sig"
	region, shard, err := c.ResolveValorantRegion(context.Background(), access, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if region != "ap" || shard != "ap" {
		t.Fatalf("region=%q shard=%q", region, shard)
	}
}
