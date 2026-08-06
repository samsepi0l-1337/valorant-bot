package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_MigratesSchema(t *testing.T) {
	s := openTemp(t)
	if s == nil {
		t.Fatal("expected store")
	}
}

func TestUpsertAndListRiotAccounts(t *testing.T) {
	s := openTemp(t)

	acc := Account{
		DiscordUserID:     "user1",
		PUUID:             "puuid-aaa",
		GameName:          "Player",
		TagLine:           "NA1",
		Region:            "na",
		Shard:             "na",
		CookiesCiphertext: []byte("cipher-blob"),
	}
	if err := s.UpsertRiotAccount(acc); err != nil {
		t.Fatalf("UpsertRiotAccount: %v", err)
	}

	list, err := s.ListRiotAccountsByDiscord("user1")
	if err != nil {
		t.Fatalf("ListRiotAccountsByDiscord: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1", len(list))
	}
	got := list[0]
	if got.PUUID != "puuid-aaa" || got.GameName != "Player" || got.TagLine != "NA1" {
		t.Errorf("got %+v", got)
	}
	if string(got.CookiesCiphertext) != "cipher-blob" {
		t.Errorf("cookies = %q", got.CookiesCiphertext)
	}
	if got.ID == 0 {
		t.Error("expected non-zero ID")
	}

	// upsert same puuid updates fields
	acc.GameName = "Updated"
	acc.CookiesCiphertext = []byte("new-cipher")
	if err := s.UpsertRiotAccount(acc); err != nil {
		t.Fatalf("UpsertRiotAccount update: %v", err)
	}
	list, err = s.ListRiotAccountsByDiscord("user1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("len after upsert = %d", len(list))
	}
	if list[0].GameName != "Updated" {
		t.Errorf("GameName = %q", list[0].GameName)
	}
	if string(list[0].CookiesCiphertext) != "new-cipher" {
		t.Errorf("cookies = %q", list[0].CookiesCiphertext)
	}
}

func TestDeleteRiotAccount(t *testing.T) {
	s := openTemp(t)
	acc := Account{
		DiscordUserID:     "user1",
		PUUID:             "puuid-del",
		CookiesCiphertext: []byte("x"),
	}
	if err := s.UpsertRiotAccount(acc); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRiotAccount("user1", "puuid-del"); err != nil {
		t.Fatalf("DeleteRiotAccount: %v", err)
	}
	list, err := s.ListRiotAccountsByDiscord("user1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d", len(list))
	}
}

func TestAuthPending_PutAndTake(t *testing.T) {
	s := openTemp(t)
	expires := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)

	if err := s.PutAuthPending("state-abc", "discord-42", expires); err != nil {
		t.Fatalf("PutAuthPending: %v", err)
	}

	uid, ok, err := s.TakeAuthPending("state-abc")
	if err != nil {
		t.Fatalf("TakeAuthPending: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if uid != "discord-42" {
		t.Errorf("uid = %q", uid)
	}

	// second take should miss
	_, ok, err = s.TakeAuthPending("state-abc")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected ok=false after take")
	}
}

func TestAuthPending_Missing(t *testing.T) {
	s := openTemp(t)
	_, ok, err := s.TakeAuthPending("nope")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected ok=false")
	}
}

func TestWishlist_AddRemoveList(t *testing.T) {
	s := openTemp(t)

	if err := s.AddWishlist("user1", "skin-1", "Reaver Vandal"); err != nil {
		t.Fatalf("AddWishlist: %v", err)
	}
	if err := s.AddWishlist("user1", "skin-2", "Prime Classic"); err != nil {
		t.Fatalf("AddWishlist: %v", err)
	}
	// idempotent add
	if err := s.AddWishlist("user1", "skin-1", "Reaver Vandal"); err != nil {
		t.Fatalf("AddWishlist idempotent: %v", err)
	}

	items, err := s.ListWishlists("user1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}

	if err := s.RemoveWishlist("user1", "skin-1"); err != nil {
		t.Fatalf("RemoveWishlist: %v", err)
	}
	items, err = s.ListWishlists("user1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SkinUUID != "skin-2" {
		t.Fatalf("got %+v", items)
	}
}

func TestListAllRiotAccounts(t *testing.T) {
	s := openTemp(t)

	all, err := s.ListAllRiotAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("empty db len = %d", len(all))
	}

	for _, acc := range []Account{
		{DiscordUserID: "user1", PUUID: "p1", GameName: "A", TagLine: "1", CookiesCiphertext: []byte("a")},
		{DiscordUserID: "user2", PUUID: "p2", GameName: "B", TagLine: "2", CookiesCiphertext: []byte("b")},
		{DiscordUserID: "user1", PUUID: "p3", GameName: "C", TagLine: "3", CookiesCiphertext: []byte("c")},
	} {
		if err := s.UpsertRiotAccount(acc); err != nil {
			t.Fatal(err)
		}
	}

	all, err = s.ListAllRiotAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	byPUUID := map[string]Account{}
	for _, a := range all {
		byPUUID[a.PUUID] = a
	}
	if byPUUID["p1"].DiscordUserID != "user1" || byPUUID["p2"].DiscordUserID != "user2" {
		t.Fatalf("got %+v", byPUUID)
	}
}

func TestListAllWishlists(t *testing.T) {
	s := openTemp(t)

	all, err := s.ListAllWishlists()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("empty db len = %d", len(all))
	}

	if err := s.AddWishlist("user1", "skin-a", "Skin A"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWishlist("user2", "skin-b", "Skin B"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWishlist("user1", "skin-c", "Skin C"); err != nil {
		t.Fatal(err)
	}

	all, err = s.ListAllWishlists()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
}

func TestGuildSettings(t *testing.T) {
	s := openTemp(t)

	gs := GuildSettings{
		GuildID:        "guild-1",
		DailyChannelID: "chan-1",
		Enabled:        true,
		DailyHour:      21,
	}
	if err := s.UpsertGuildSettings(gs); err != nil {
		t.Fatalf("UpsertGuildSettings: %v", err)
	}

	got, ok, err := s.GetGuildSettings("guild-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected found")
	}
	if got.DailyChannelID != "chan-1" || !got.Enabled || got.DailyHour != 21 {
		t.Errorf("got %+v", got)
	}

	_, ok, err = s.GetGuildSettings("missing")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected not found")
	}

	if err := s.UpsertGuildSettings(GuildSettings{
		GuildID:        "guild-2",
		DailyChannelID: "chan-2",
		Enabled:        false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertGuildSettings(GuildSettings{
		GuildID:        "guild-3",
		DailyChannelID: "chan-3",
		Enabled:        true,
	}); err != nil {
		t.Fatal(err)
	}

	enabled, err := s.ListEnabledGuildSettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 2 {
		t.Fatalf("enabled len = %d, want 2", len(enabled))
	}
}

func TestOpen_AppliesBusyTimeoutAndWAL(t *testing.T) {
	s := openTemp(t)
	var busy int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy < 5000 {
		t.Fatalf("busy_timeout=%d, want >= 5000", busy)
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode=%q, want wal", mode)
	}
	if s.db.Stats().MaxOpenConnections != 1 {
		t.Fatalf("MaxOpenConnections=%d, want 1 (serialize Discord handlers)", s.db.Stats().MaxOpenConnections)
	}
}

func TestConcurrentMixedOps_NoBusy(t *testing.T) {
	s := openTemp(t)
	const writers = 8
	const iters = 40

	var busyHits atomic.Int64
	var otherErrs atomic.Int64
	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			uid := fmt.Sprintf("user-%d", id)
			for i := 0; i < iters; i++ {
				acc := Account{
					DiscordUserID:     uid,
					PUUID:             fmt.Sprintf("puuid-%d-%d", id, i%3),
					GameName:          "N",
					TagLine:           "T",
					Region:            "kr",
					Shard:             "kr",
					CookiesCiphertext: []byte("x"),
				}
				if err := s.UpsertRiotAccount(acc); err != nil {
					if isBusy(err) {
						busyHits.Add(1)
					} else {
						otherErrs.Add(1)
					}
				}
				if _, err := s.ListRiotAccountsByDiscord(uid); err != nil {
					if isBusy(err) {
						busyHits.Add(1)
					} else {
						otherErrs.Add(1)
					}
				}
				if err := s.SetUserLanguage(uid, "ko"); err != nil {
					if isBusy(err) {
						busyHits.Add(1)
					} else {
						otherErrs.Add(1)
					}
				}
				if _, err := s.GetUserLanguage(uid); err != nil {
					if isBusy(err) {
						busyHits.Add(1)
					} else {
						otherErrs.Add(1)
					}
				}
				if err := s.PutAuthPending(fmt.Sprintf("st-%d-%d", id, i), uid, time.Now().Add(time.Minute)); err != nil {
					if isBusy(err) {
						busyHits.Add(1)
					} else {
						otherErrs.Add(1)
					}
				}
			}
		}(w)
	}
	wg.Wait()

	if busyHits.Load() != 0 {
		t.Fatalf("got %d SQLITE_BUSY errors under concurrent load", busyHits.Load())
	}
	if otherErrs.Load() != 0 {
		t.Fatalf("got %d unexpected store errors", otherErrs.Load())
	}
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}
