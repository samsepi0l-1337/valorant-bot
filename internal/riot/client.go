package riot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	HTTPClient *http.Client

	AuthBaseURL         string
	EntitlementsBaseURL string
	GeoBaseURL          string
	PDBaseURLFunc       func(shard string) string
	ClientVersion       string
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		HTTPClient:          httpClient,
		AuthBaseURL:         "https://auth.riotgames.com",
		EntitlementsBaseURL: "https://entitlements.auth.riotgames.com",
		GeoBaseURL:          "https://riot-geo.pas.si.riotgames.com",
		PDBaseURLFunc: func(shard string) string {
			return fmt.Sprintf("https://pd.%s.a.pvp.net", shard)
		},
		ClientVersion: DefaultClientVersion,
	}
}

func (c *Client) pdBase(shard string) string {
	if c.PDBaseURLFunc != nil {
		return c.PDBaseURLFunc(shard)
	}
	return fmt.Sprintf("https://pd.%s.a.pvp.net", shard)
}

func (c *Client) clientVersion() string {
	if c.ClientVersion != "" {
		return c.ClientVersion
	}
	return DefaultClientVersion
}

func (c *Client) GetEntitlements(ctx context.Context, accessToken string) (string, error) {
	url := strings.TrimSuffix(c.EntitlementsBaseURL, "/") + "/api/token/v1"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		return "", fmt.Errorf("entitlements: %s", resp.Status)
	}

	var out struct {
		EntitlementsToken string `json:"entitlements_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.EntitlementsToken == "" {
		return "", fmt.Errorf("entitlements: empty token")
	}
	return out.EntitlementsToken, nil
}

func (c *Client) GetUserInfo(ctx context.Context, accessToken string) (string, error) {
	url := strings.TrimSuffix(c.AuthBaseURL, "/") + "/userinfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		return "", fmt.Errorf("userinfo: %s", resp.Status)
	}

	var out struct {
		Sub string `json:"sub"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Sub == "" {
		return "", fmt.Errorf("userinfo: missing sub")
	}
	return out.Sub, nil
}

func (c *Client) GetPlayerNames(ctx context.Context, accessToken, entitlementsToken, shard string, puuids []string) ([]PlayerName, error) {
	url := c.pdBase(shard) + "/name-service/v2/players"
	body, err := json.Marshal(puuids)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Riot-Entitlements-JWT", entitlementsToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Riot-ClientPlatform", DefaultClientPlatform)
	req.Header.Set("X-Riot-ClientVersion", c.clientVersion())

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		return nil, fmt.Errorf("name-service: %s", resp.Status)
	}

	var raw []struct {
		Subject  string `json:"Subject"`
		GameName string `json:"GameName"`
		TagLine  string `json:"TagLine"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	names := make([]PlayerName, 0, len(raw))
	for _, r := range raw {
		names = append(names, PlayerName{
			PUUID:    r.Subject,
			GameName: r.GameName,
			TagLine:  r.TagLine,
		})
	}
	return names, nil
}

func (c *Client) GetStorefront(ctx context.Context, accessToken, entitlementsToken, shard, puuid string) (Storefront, error) {
	url := c.pdBase(shard) + "/store/v3/storefront/" + puuid
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return Storefront{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Riot-Entitlements-JWT", entitlementsToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Riot-ClientPlatform", DefaultClientPlatform)
	req.Header.Set("X-Riot-ClientVersion", c.clientVersion())

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return Storefront{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		return Storefront{}, fmt.Errorf("storefront: %s", resp.Status)
	}

	var raw struct {
		SkinsPanelLayout struct {
			SingleItemStoreOffers                      []offerJSON `json:"SingleItemStoreOffers"`
			SingleItemOffers                           []string    `json:"SingleItemOffers"`
			RemainingDurationInSeconds                 int         `json:"RemainingDurationInSeconds"`
			SingleItemOffersRemainingDurationInSeconds int         `json:"SingleItemOffersRemainingDurationInSeconds"`
		} `json:"SkinsPanelLayout"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return Storefront{}, err
	}

	sf := Storefront{
		Offers: make([]ShopOffer, 0, 4),
	}
	remaining := raw.SkinsPanelLayout.SingleItemOffersRemainingDurationInSeconds
	if remaining == 0 {
		remaining = raw.SkinsPanelLayout.RemainingDurationInSeconds
	}
	if remaining > 0 {
		sf.RemainsUntilReset = time.Duration(remaining) * time.Second
	}

	if len(raw.SkinsPanelLayout.SingleItemStoreOffers) > 0 {
		for _, o := range raw.SkinsPanelLayout.SingleItemStoreOffers {
			sf.Offers = append(sf.Offers, o.toShopOffer())
		}
		return sf, nil
	}

	// Older/partial payloads: SingleItemOffers is just skin/offer UUID strings.
	for _, id := range raw.SkinsPanelLayout.SingleItemOffers {
		if id == "" {
			continue
		}
		sf.Offers = append(sf.Offers, ShopOffer{SkinUUID: id})
	}
	return sf, nil
}

type offerJSON struct {
	OfferID string         `json:"OfferID"`
	ItemID  string         `json:"ItemID"`
	Cost    map[string]int `json:"Cost"`
	Rewards []struct {
		ItemID string `json:"ItemID"`
	} `json:"Rewards"`
}

func (o offerJSON) toShopOffer() ShopOffer {
	id := o.OfferID
	if id == "" {
		id = o.ItemID
	}
	if id == "" && len(o.Rewards) > 0 {
		id = o.Rewards[0].ItemID
	}
	cost := 0
	if o.Cost != nil {
		cost = o.Cost[VPCurrencyUUID]
	}
	return ShopOffer{SkinUUID: id, CostVP: cost}
}

func (c *Client) CookieReauth(ctx context.Context, cookieHeader string) (accessToken, idToken string, err error) {
	authURL := strings.TrimSuffix(c.AuthBaseURL, "/") + "/authorize?redirect_uri=https%3A%2F%2Fplayvalorant.com%2Fopt_in&client_id=play-valorant-web-prod&response_type=token%20id_token&nonce=1&scope=account%20openid"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Cookie", cookieHeader)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", "", fmt.Errorf("cookie reauth: missing Location header")
	}
	return ParseRedirectURL(loc)
}

// GetValorantAffinity asks Riot Geo which live Valorant region the account uses.
// This is the authoritative source for ap vs kr (and other shards).
func (c *Client) GetValorantAffinity(ctx context.Context, accessToken, idToken string) (string, error) {
	if accessToken == "" || idToken == "" {
		return "", fmt.Errorf("geo: access token and id token required")
	}
	base := c.GeoBaseURL
	if base == "" {
		base = "https://riot-geo.pas.si.riotgames.com"
	}
	url := strings.TrimSuffix(base, "/") + "/pas/v1/product/valorant"
	body, err := json.Marshal(map[string]string{"id_token": idToken})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		return "", fmt.Errorf("geo: %s", resp.Status)
	}
	var out struct {
		Affinities struct {
			Live string `json:"live"`
		} `json:"affinities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	live := NormalizeRegion(out.Affinities.Live)
	if live == "" {
		return "", fmt.Errorf("geo: unknown live affinity %q", out.Affinities.Live)
	}
	return live, nil
}

// ResolveValorantRegion prefers Riot Geo (when idToken is present), then the
// access-token dat.r claim, then an explicit fallback. Never assumes "kr".
func (c *Client) ResolveValorantRegion(ctx context.Context, accessToken, idToken, fallback string) (region, shard string, err error) {
	if idToken != "" {
		if live, gerr := c.GetValorantAffinity(ctx, accessToken, idToken); gerr == nil {
			shard, err = ShardForRegion(live)
			if err != nil {
				return "", "", err
			}
			return live, shard, nil
		}
	}
	if region, shard, err = RegionFromToken(accessToken); err == nil {
		return region, shard, nil
	}
	fb := NormalizeRegion(fallback)
	if fb == "" {
		return "", "", fmt.Errorf("resolve region: %w", err)
	}
	shard, serr := ShardForRegion(fb)
	if serr != nil {
		return "", "", serr
	}
	return fb, shard, nil
}
