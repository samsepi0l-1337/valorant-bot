package valorantbot

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// InvitePath is the local HTTP path that redirects to the Discord invite URL.
const InvitePath = "/invite"

// InviteURL builds the Discord OAuth authorize page where a user picks a server
// and adds the bot (same form as the Developer Portal invite link).
// Example: https://discord.com/oauth2/authorize?client_id=APP_ID
func InviteURL(appID string) string {
	return "https://discord.com/oauth2/authorize?client_id=" + url.QueryEscape(appID)
}

func inviteRedirect(appID string) http.HandlerFunc {
	target := InviteURL(appID)
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}
}

func formatInviteLog(appID, authBaseURL string) string {
	base := strings.TrimRight(authBaseURL, "/")
	return fmt.Sprintf("invite: %s  (or open %s%s)", InviteURL(appID), base, InvitePath)
}
