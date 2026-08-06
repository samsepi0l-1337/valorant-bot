package skins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureLoadedAndGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/weapons/skins" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.URL.Query().Get("language") != LangKO {
			t.Errorf("language %q", r.URL.Query().Get("language"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 200,
			"data": []map[string]any{
				{
					"uuid":        "skin-1",
					"displayName": "프라임 밴달",
					"displayIcon": "https://example.com/icon.png",
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
					"uuid":        "skin-2",
					"displayName": "리버 팬텀",
					"displayIcon": "https://example.com/r.png",
				},
			},
		})
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

	chroma, ok := c.Get("chroma-1", "ko")
	if !ok || chroma.DisplayIcon != "https://example.com/full.png" {
		t.Fatalf("chroma %+v %v", chroma, ok)
	}

	_, ok = c.Get("missing", "ko")
	if ok {
		t.Fatal("expected miss")
	}
}

func TestSearchByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"uuid": "a", "displayName": "프라임 밴달", "displayIcon": ""},
				{"uuid": "b", "displayName": "프라임 클래식", "displayIcon": ""},
				{"uuid": "c", "displayName": "리버", "displayIcon": ""},
			},
		})
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer srv.Close()

	c := NewCache(srv.Client(), srv.URL)
	_ = c.EnsureLoaded(context.Background(), "en")

	results := c.SearchByName("match", "en")
	if len(results) > 25 {
		t.Fatalf("expected at most 25, got %d", len(results))
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
