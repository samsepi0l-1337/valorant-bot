package riot

import (
	"errors"
	"net/url"
	"strings"
)

// ParseRedirectURL extracts tokens from a Riot OAuth redirect URL.
// Supports https://playvalorant.com/ko-kr/opt_in/#access_token=... and query forms.
func ParseRedirectURL(rawURL string) (accessToken, idToken string, err error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", err
	}

	vals := u.Query()
	if u.Fragment != "" {
		frag, ferr := url.ParseQuery(u.Fragment)
		if ferr == nil && (frag.Get("access_token") != "" || len(vals) == 0) {
			vals = frag
		}
	}

	accessToken = vals.Get("access_token")
	idToken = vals.Get("id_token")
	if accessToken == "" {
		return "", "", errors.New("redirect URL missing access_token")
	}
	return accessToken, idToken, nil
}
