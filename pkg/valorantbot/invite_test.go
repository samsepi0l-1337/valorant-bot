package valorantbot

import (
	"strings"
	"testing"
)

func TestInviteURL(t *testing.T) {
	got := InviteURL("1534895506323537951")
	want := "https://discord.com/oauth2/authorize?client_id=1534895506323537951"
	if got != want {
		t.Fatalf("InviteURL = %q, want %q", got, want)
	}
}

func TestFormatInviteLog(t *testing.T) {
	got := formatInviteLog("123", "http://127.0.0.1:8787/")
	if !strings.Contains(got, "https://discord.com/oauth2/authorize?client_id=123") {
		t.Fatalf("missing invite url: %s", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:8787/invite") {
		t.Fatalf("missing local invite path: %s", got)
	}
}
