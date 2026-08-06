package riot

import "time"

const (
	DefaultClientPlatform = "ew0KCSJwbGF0Zm9ybVR5cGUiOiAiUEMiLA0KCSJwbGF0Zm9ybU9TIjogIldpbmRvd3MiLA0KCSJwbGF0Zm9ybU9TVmVyc2lvbiI6ICIxMC4wLjE5MDQyLjEuMjU2LjY0Yml0IiwNCgkicGxhdGZvcm1DaGlwc2V0IjogIlVua25vd24iDQp9"
	DefaultClientVersion  = "release-13.02-shipping-10-5229475"
	VPCurrencyUUID        = "85ad13f7-3d1b-5128-9eb2-7cd8ee0b5741"
)

type Tokens struct {
	AccessToken       string
	IDToken           string
	EntitlementsToken string
}

type PlayerName struct {
	PUUID    string
	GameName string
	TagLine  string
}

type ShopOffer struct {
	SkinUUID string
	CostVP   int
}

type Storefront struct {
	Offers            []ShopOffer
	RemainsUntilReset time.Duration
}
