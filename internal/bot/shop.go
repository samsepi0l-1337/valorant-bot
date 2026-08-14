package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/dosfsociety/valorant-bot/internal/crypto"
	"github.com/dosfsociety/valorant-bot/internal/riot"
	"github.com/dosfsociety/valorant-bot/internal/skins"
	"github.com/dosfsociety/valorant-bot/internal/store"
)

// RiotAPI is the subset of riot.Client used by ShopFetcher (mock in tests).
type RiotAPI interface {
	CookieReauth(ctx context.Context, cookieHeader string) (accessToken, idToken string, err error)
	GetEntitlements(ctx context.Context, accessToken string) (string, error)
	GetStorefront(ctx context.Context, accessToken, entitlementsToken, shard, puuid string) (riot.Storefront, error)
}

// ShopFetcher loads shops for all accounts belonging to a Discord user.
type ShopFetcher struct {
	Accounts AccountStore
	Boxer    *crypto.Boxer
	Riot     RiotAPI
	Skins    *skins.Cache
}

// ShopsForUser implements ShopService. Accounts are fetched concurrently
// (order preserved) so N accounts take roughly one round-trip instead of N.
func (f *ShopFetcher) ShopsForUser(ctx context.Context, discordUserID, language string) ([]AccountShop, error) {
	if f.Accounts == nil {
		return nil, fmt.Errorf("accounts not configured")
	}
	accounts, err := f.Accounts.ListRiotAccountsByDiscord(discordUserID)
	if err != nil {
		return nil, err
	}

	// Load skin metadata once up front rather than once per account.
	var skinsErr error
	if f.Skins != nil {
		skinsErr = f.Skins.EnsureLoaded(ctx, language)
	}

	out := make([]AccountShop, len(accounts))
	var wg sync.WaitGroup
	for idx, acc := range accounts {
		wg.Add(1)
		go func(idx int, acc store.Account) {
			defer wg.Done()
			out[idx] = f.shopForAccount(ctx, acc, language, skinsErr)
		}(idx, acc)
	}
	wg.Wait()
	return out, nil
}

func (f *ShopFetcher) shopForAccount(ctx context.Context, acc store.Account, language string, skinsErr error) AccountShop {
	view := AccountShop{
		GameName: acc.GameName,
		TagLine:  acc.TagLine,
		Region:   acc.Region,
		PUUID:    acc.PUUID,
	}
	if f.Boxer == nil || f.Riot == nil {
		view.Err = "shop fetcher not configured"
		return view
	}
	plain, err := f.Boxer.Decrypt(acc.CookiesCiphertext)
	if err != nil {
		view.Err = "저장된 세션을 복호화할 수 없습니다. `/auth` 로 다시 연결하세요."
		return view
	}
	access, err := f.resolveAccessToken(ctx, string(plain))
	if err != nil {
		view.Err = err.Error()
		return view
	}
	ent, err := f.Riot.GetEntitlements(ctx, access)
	if err != nil {
		view.Err = fmt.Sprintf("권한 토큰을 가져오지 못했습니다: %v", err)
		return view
	}
	shard := acc.Shard
	if shard == "" {
		shard = acc.Region
	}
	sf, err := f.Riot.GetStorefront(ctx, access, ent, shard, acc.PUUID)
	if err != nil {
		view.Err = fmt.Sprintf("상점을 불러오지 못했습니다: %v", err)
		return view
	}
	if skinsErr != nil {
		view.Err = fmt.Sprintf("스킨 데이터를 불러오지 못했습니다: %v", skinsErr)
		return view
	}
	for _, offer := range sf.Offers {
		ov := OfferView{
			SkinUUID: offer.SkinUUID,
			CostVP:   offer.CostVP,
		}
		if f.Skins != nil {
			if skin, ok := f.Skins.Get(offer.SkinUUID, language); ok {
				ov.SkinUUID = skin.UUID
				ov.DisplayName = skin.DisplayName
				ov.IconURL = skin.DisplayIcon
				ov.Color = skin.Color
			}
		}
		if ov.DisplayName == "" {
			ov.DisplayName = offer.SkinUUID
		}
		view.Offers = append(view.Offers, ov)
	}
	return view
}

func (f *ShopFetcher) resolveAccessToken(ctx context.Context, session string) (string, error) {
	session = strings.TrimSpace(session)
	if strings.HasPrefix(session, "access_token=") {
		tok := strings.TrimPrefix(session, "access_token=")
		if tok == "" {
			return "", fmt.Errorf("저장된 access_token이 비어 있습니다. `/auth` 로 다시 연결하세요.")
		}
		return tok, nil
	}
	access, _, err := f.Riot.CookieReauth(ctx, session)
	if err != nil {
		return "", fmt.Errorf("세션이 만료되었습니다. `/auth` 로 다시 로그인하세요.")
	}
	return access, nil
}
