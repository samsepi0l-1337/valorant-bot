package riot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// FetchClientVersion loads the current X-Riot-ClientVersion from valorant-api.com.
func FetchClientVersion(ctx context.Context, httpClient *http.Client) (string, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://valorant-api.com/v1/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("version api: %s", resp.Status)
	}
	var out struct {
		Data struct {
			RiotClientVersion string `json:"riotClientVersion"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Data.RiotClientVersion == "" {
		return "", fmt.Errorf("version api: empty riotClientVersion")
	}
	return out.Data.RiotClientVersion, nil
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
