package riot

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// NormalizeRegion maps Riot platform / affinity aliases onto Valorant region IDs.
// Unknown inputs return "".
func NormalizeRegion(region string) string {
	r := strings.ToLower(strings.TrimSpace(region))
	switch r {
	case "ap", "ap1", "asia", "apac", "sea":
		return "ap"
	case "kr", "kr1":
		return "kr"
	case "na", "na1":
		return "na"
	case "latam":
		return "latam"
	case "br":
		return "br"
	case "eu", "euw", "euw1", "eun", "eun1", "ru", "tr":
		return "eu"
	default:
		return ""
	}
}

func ShardForRegion(region string) (string, error) {
	switch NormalizeRegion(region) {
	case "na", "latam", "br":
		return "na", nil
	case "eu":
		return "eu", nil
	case "ap":
		return "ap", nil
	case "kr":
		return "kr", nil
	default:
		return "", fmt.Errorf("unknown region %q", region)
	}
}

func RegionFromToken(accessToken string) (region, shard string, err error) {
	payload, err := jwtPayload(accessToken)
	if err != nil {
		return "", "", err
	}

	var claims struct {
		Dat struct {
			R string `json:"r"`
		} `json:"dat"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", err
	}

	raw := strings.TrimSpace(claims.Dat.R)
	region = NormalizeRegion(raw)
	if region == "" {
		return "", "", fmt.Errorf("access token missing/unknown region claim dat.r=%q", raw)
	}

	shard, err = ShardForRegion(region)
	if err != nil {
		return "", "", err
	}
	return region, shard, nil
}

// DisplayRegion returns a user-facing region label for Discord messages.
// lang is "ko" or "en" (empty defaults to English).
func DisplayRegion(region, lang string) string {
	r := NormalizeRegion(region)
	if r == "" {
		r = strings.ToLower(strings.TrimSpace(region))
	}
	if lang == "ko" {
		switch r {
		case "ap":
			return "아시아 (ap)"
		case "kr":
			return "한국 (kr)"
		case "na":
			return "북미 (na)"
		case "latam":
			return "라틴아메리카 (latam)"
		case "br":
			return "브라질 (br)"
		case "eu":
			return "유럽 (eu)"
		}
	} else {
		switch r {
		case "ap":
			return "Asia Pacific (ap)"
		case "kr":
			return "Korea (kr)"
		case "na":
			return "North America (na)"
		case "latam":
			return "Latin America (latam)"
		case "br":
			return "Brazil (br)"
		case "eu":
			return "Europe (eu)"
		}
	}
	if r == "" {
		return region
	}
	return r
}

func jwtPayload(token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, errors.New("invalid JWT")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	return data, nil
}
