package skins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func tierHandler(t *testing.T, skinsPayload map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/contenttiers":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{
					{"uuid": "tier-select", "highlightColor": "5a9fe2ff"},
					{"uuid": "tier-deluxe", "highlightColor": "009587ff"},
					{"uuid": "tier-premium", "highlightColor": "d1548dff"},
					{"uuid": "tier-exclusive", "highlightColor": "f5955bff"},
					{"uuid": "tier-ultra", "highlightColor": "fad663ff"},
				},
			})
		case "/v1/weapons/skins":
			if r.URL.Query().Get("language") != LangKO && r.URL.Query().Get("language") != LangEN {
				t.Errorf("language %q", r.URL.Query().Get("language"))
			}
			_ = json.NewEncoder(w).Encode(skinsPayload)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func TestEnsureLoadedAndGet(t *testing.T) {
	srv := httptest.NewServer(tierHandler(t, map[string]any{
		"status": 200,
		"data": []map[string]any{
			{
				"uuid":             "skin-1",
				"displayName":      "프라임 밴달",
				"displayIcon":      "https://example.com/icon.png",
				"contentTierUuid":  "tier-exclusive",
				"levels": []map[string]string{
					{
						"uuid":        "level-1",
						"displayName": "프라임 밴달",
						"displayIcon": "https://example.com/level.png",
					},
				},
				"chromas": []map[string]string{
					{
						"uuid":        "chroma-1",
						"displayName": "프라임 밴달 Level 2\nOrange",
						"displayIcon": "https://example.com/chroma.png",
						"fullRender":  "https://example.com/full.png",
					},
				},
			},
			{
				"uuid":            "skin-2",
				"displayName":     "리버 팬텀",
				"displayIcon":     "https://example.com/r.png",
				"contentTierUuid": "tier-select",
			},
		},
	}))
	defer srv.Close()

	c := NewCache(srv.Client(), srv.URL)
	if err := c.EnsureLoaded(context.Background(), "ko"); err != nil {
		t.Fatal(err)
	}

	s, ok := c.Get("skin-1", "ko")
	if !ok || s.DisplayName != "프라임 밴달" {
		t.Fatalf("%+v %v", s, ok)
	}
	if s.Color != 0xf5955b {
		t.Fatalf("exclusive color %#x", s.Color)
	}

	lvl, ok := c.Get("level-1", LangKO)
	if !ok {
		t.Fatal("expected level UUID lookup")
	}
	if lvl.DisplayName != "프라임 밴달" {
		t.Fatalf("level name %+v", lvl)
	}
	if lvl.DisplayIcon != "https://example.com/level.png" {
		t.Fatalf("level icon %+v", lvl)
	}
	if lvl.UUID != "skin-1" {
		t.Fatalf("parent uuid %+v", lvl)
	}
	if lvl.Color != 0xf5955b {
		t.Fatalf("level color %#x", lvl.Color)
	}

	chroma, ok := c.Get("chroma-1", "ko")
	if !ok || chroma.DisplayIcon != "https://example.com/full.png" {
		t.Fatalf("chroma %+v %v", chroma, ok)
	}
	if chroma.Color != 0xf5955b {
		t.Fatalf("chroma color %#x", chroma.Color)
	}

	selectSkin, ok := c.Get("skin-2", "ko")
	if !ok || selectSkin.Color != 0x5a9fe2 {
		t.Fatalf("select skin %+v %v", selectSkin, ok)
	}

	_, ok = c.Get("missing", "ko")
	if ok {
		t.Fatal("expected miss")
	}
}

func TestSearchByName(t *testing.T) {
	srv := httptest.NewServer(tierHandler(t, map[string]any{
		"data": []map[string]string{
			{"uuid": "a", "displayName": "프라임 밴달", "displayIcon": ""},
			{"uuid": "b", "displayName": "프라임 클래식", "displayIcon": ""},
			{"uuid": "c", "displayName": "리버", "displayIcon": ""},
		},
	}))
	defer srv.Close()

	c := NewCache(srv.Client(), srv.URL)
	_ = c.EnsureLoaded(context.Background(), "ko")

	results := c.SearchByName("프라임", "ko")
	if len(results) != 2 {
		t.Fatalf("got %d results: %+v", len(results), results)
	}
}

func TestSearchByName_Limit(t *testing.T) {
	var items []map[string]string
	for i := 0; i < 20; i++ {
		items = append(items, map[string]string{
			"uuid":        "u",
			"displayName": "Match Skin",
			"displayIcon": "",
		})
	}
	srv := httptest.NewServer(tierHandler(t, map[string]any{"data": items}))
	defer srv.Close()

	c := NewCache(srv.Client(), srv.URL)
	_ = c.EnsureLoaded(context.Background(), "en")

	results := c.SearchByName("match", "en")
	if len(results) > 25 {
		t.Fatalf("expected at most 25, got %d", len(results))
	}
}

func TestParseHighlightColor(t *testing.T) {
	tests := []struct {
		raw  string
		want int
		ok   bool
	}{
		{"5a9fe2ff", 0x5a9fe2, true},
		{"#d1548dAA", 0xd1548d, true},
		{"fad663", 0xfad663, true},
		{"zz", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := ParseHighlightColor(tt.raw)
		if ok != tt.ok || got != tt.want {
			t.Fatalf("%q: got %#x %v, want %#x %v", tt.raw, got, ok, tt.want, tt.ok)
		}
	}
}

func TestNormalizeLang(t *testing.T) {
	if NormalizeLang("ko") != LangKO {
		t.Fatal(NormalizeLang("ko"))
	}
	if NormalizeLang("en") != LangEN {
		t.Fatal(NormalizeLang("en"))
	}
}
