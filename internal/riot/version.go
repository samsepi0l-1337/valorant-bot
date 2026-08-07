package riot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ClientMeta is the Riot Client version pair from valorant-api.com.
type ClientMeta struct {
	RiotClientVersion string
	RiotClientBuild   string
}

// FetchClientVersion loads the current X-Riot-ClientVersion from valorant-api.com.
func FetchClientVersion(ctx context.Context, httpClient *http.Client) (string, error) {
	meta, err := FetchClientMeta(ctx, httpClient)
	if err != nil {
		return "", err
	}
	return meta.RiotClientVersion, nil
}

// FetchClientMeta loads riotClientVersion + riotClientBuild from valorant-api.com.
func FetchClientMeta(ctx context.Context, httpClient *http.Client) (ClientMeta, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://valorant-api.com/v1/version", nil)
	if err != nil {
		return ClientMeta{}, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return ClientMeta{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ClientMeta{}, fmt.Errorf("version api: %s", resp.Status)
	}
	var out struct {
		Data struct {
			RiotClientVersion string `json:"riotClientVersion"`
			RiotClientBuild   string `json:"riotClientBuild"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ClientMeta{}, err
	}
	if out.Data.RiotClientVersion == "" {
		return ClientMeta{}, fmt.Errorf("version api: empty riotClientVersion")
	}
	return ClientMeta{
		RiotClientVersion: out.Data.RiotClientVersion,
		RiotClientBuild:   out.Data.RiotClientBuild,
	}, nil
}

// GameNameFromIDToken reads acct.game_name / acct.tag_line from an id_token JWT.
func GameNameFromIDToken(idToken string) (gameName, tagLine string, err error) {
	payload, err := jwtPayload(idToken)
	if err != nil {
		return "", "", err
	}
	var claims struct {
		Acct struct {
			GameName string `json:"game_name"`
			TagLine  string `json:"tag_line"`
		} `json:"acct"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", err
	}
	if claims.Acct.GameName == "" {
		return "", "", fmt.Errorf("id_token missing acct.game_name")
	}
	return claims.Acct.GameName, claims.Acct.TagLine, nil
}
