package riot

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func ShardForRegion(region string) (string, error) {
	switch strings.ToLower(region) {
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

	region = strings.ToLower(strings.TrimSpace(claims.Dat.R))
	if region == "" {
		return "", "", errors.New("access token missing region claim (dat.r); set region manually or use ShardForRegion")
	}

	shard, err = ShardForRegion(region)
	if err != nil {
		return "", "", err
	}
	return region, shard, nil
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
