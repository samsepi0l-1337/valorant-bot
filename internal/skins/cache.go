package skins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const defaultBaseURL = "https://valorant-api.com"

// API language codes for valorant-api.com (?language=).
const (
	LangKO = "ko-KR"
	LangEN = "en-US"
)

// Skin is a displayable weapon skin (resolved from skin, level, or chroma UUID).
type Skin struct {
	UUID        string // parent skin UUID
	DisplayName string
	DisplayIcon string
}

type langIndex struct {
	byUUID map[string]Skin
	all    []Skin
}

type Cache struct {
	client  *http.Client
	baseURL string

	mu    sync.RWMutex
	langs map[string]*langIndex // API language → index
}

func NewCache(client *http.Client, baseURL string) *Cache {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Cache{
		client:  client,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		langs:   make(map[string]*langIndex),
	}
}

// NormalizeLang maps bot UI codes (ko/en) to valorant-api language query values.
func NormalizeLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "en", "en-us", "english":
		return LangEN
	case "ko", "ko-kr", "kr", "korean":
		return LangKO
	default:
		if lang == LangEN || lang == LangKO {
			return lang
		}
		return LangKO
	}
}

// EnsureLoaded fetches and indexes skins for the given language (idempotent per lang).
func (c *Cache) EnsureLoaded(ctx context.Context, language string) error {
	language = NormalizeLang(language)

	c.mu.RLock()
	if c.langs[language] != nil {
		c.mu.RUnlock()
		return nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.langs[language] != nil {
		return nil
	}

	u, err := url.Parse(c.baseURL + "/v1/weapons/skins")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("language", language)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("skins API: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Data []struct {
			UUID        string `json:"uuid"`
			DisplayName string `json:"displayName"`
			DisplayIcon string `json:"displayIcon"`
			Levels      []struct {
				UUID        string `json:"uuid"`
				DisplayName string `json:"displayName"`
				DisplayIcon string `json:"displayIcon"`
			} `json:"levels"`
			Chromas []struct {
				UUID        string `json:"uuid"`
				DisplayName string `json:"displayName"`
				DisplayIcon string `json:"displayIcon"`
				FullRender  string `json:"fullRender"`
			} `json:"chromas"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}

	idx := &langIndex{
		byUUID: make(map[string]Skin, len(payload.Data)*4),
		all:    make([]Skin, 0, len(payload.Data)),
	}

	for _, s := range payload.Data {
		parentIcon := s.DisplayIcon
		if parentIcon == "" && len(s.Levels) > 0 {
			parentIcon = s.Levels[0].DisplayIcon
		}
		if parentIcon == "" && len(s.Chromas) > 0 {
			parentIcon = firstNonEmpty(s.Chromas[0].FullRender, s.Chromas[0].DisplayIcon)
		}

		parent := Skin{
			UUID:        s.UUID,
			DisplayName: s.DisplayName,
			DisplayIcon: parentIcon,
		}
		idx.byUUID[s.UUID] = parent
		idx.all = append(idx.all, parent)

		for _, lvl := range s.Levels {
			icon := firstNonEmpty(lvl.DisplayIcon, parentIcon)
			idx.byUUID[lvl.UUID] = Skin{
				UUID:        s.UUID,
				DisplayName: s.DisplayName,
				DisplayIcon: icon,
			}
		}
		for _, chroma := range s.Chromas {
			icon := firstNonEmpty(chroma.FullRender, chroma.DisplayIcon, parentIcon)
			name := s.DisplayName
			if chroma.DisplayName != "" && !isStandardChromaName(chroma.DisplayName) {
				name = chroma.DisplayName
			}
			idx.byUUID[chroma.UUID] = Skin{
				UUID:        s.UUID,
				DisplayName: name,
				DisplayIcon: icon,
			}
		}
	}
	c.langs[language] = idx
	return nil
}

func isStandardChromaName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "standard", "기본", "デフォルト":
		return true
	default:
		return false
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (c *Cache) Get(uuid, language string) (Skin, bool) {
	language = NormalizeLang(language)
	c.mu.RLock()
	defer c.mu.RUnlock()
	idx := c.langs[language]
	if idx == nil {
		return Skin{}, false
	}
	s, ok := idx.byUUID[uuid]
	return s, ok
}

func (c *Cache) SearchByName(query, language string) []Skin {
	language = NormalizeLang(language)
	c.mu.RLock()
	defer c.mu.RUnlock()

	idx := c.langs[language]
	if idx == nil {
		return nil
	}

	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}

	const limit = 25 // Discord select menu max options
	out := make([]Skin, 0, limit)
	for _, s := range idx.all {
		if strings.Contains(strings.ToLower(s.DisplayName), q) {
			out = append(out, s)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}
