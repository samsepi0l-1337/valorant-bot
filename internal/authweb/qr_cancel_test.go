package authweb

import (
	"context"
	"errors"
	"testing"
)

// Mutation caught: removing only the watcher cannot clean up the live Riot QR
// session or its persisted pending row; cancellation must be owner-bound.
func TestCancelQRAuthIsOwnerBoundAndRemovesSessionAndPendingRow(t *testing.T) {
	st := newMockStore()
	deps := testDeps(st, &mockRiot{}, &mockBoxer{})
	deps.QRAuth = &mockQRAuth{pollsUntilDone: 1}
	s := New(deps)
	_, state, err := s.BeginQRAuth(context.Background(), "owner-1")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.CancelQRAuth(state, "intruder-1"); !errors.Is(err, ErrQROwner) {
		t.Fatalf("wrong-owner cancellation error=%v, want ErrQROwner", err)
	}
	s.mu.Lock()
	_, liveAfterIntruder := s.qrSessions[state]
	s.mu.Unlock()
	if !liveAfterIntruder {
		t.Fatal("wrong owner removed the live QR session")
	}
	if owner, ok := st.pending[state]; !ok || owner != "owner-1" {
		t.Fatalf("wrong owner changed persisted pending row: owner=%q exists=%v", owner, ok)
	}

	if err := s.CancelQRAuth(state, "owner-1"); err != nil {
		t.Fatalf("owner cancellation: %v", err)
	}
	s.mu.Lock()
	_, liveAfterOwner := s.qrSessions[state]
	s.mu.Unlock()
	if liveAfterOwner {
		t.Fatal("owner cancellation retained the live QR session")
	}
	if _, ok := st.pending[state]; ok {
		t.Fatal("owner cancellation retained the persisted pending row")
	}
	if err := s.CancelQRAuth(state, "owner-1"); err != nil {
		t.Fatalf("repeated owner cancellation should be idempotent: %v", err)
	}
}

// Mutation caught: authorizing cancellation only through the in-memory QR map
// makes shutdown/map loss turn owner cleanup into a no-op that orphans storage.
func TestCancelQRAuthDeletesOnlyOwnerPendingRowAfterSessionMapLoss(t *testing.T) {
	st := newMockStore()
	deps := testDeps(st, &mockRiot{}, &mockBoxer{})
	deps.QRAuth = &mockQRAuth{pollsUntilDone: 1}
	s := New(deps)
	_, state, err := s.BeginQRAuth(context.Background(), "owner-1")
	if err != nil {
		t.Fatal(err)
	}

	// Reproduce the shutdown boundary that drains volatile QR sessions before
	// the Discord handler receives its definite initial-edit rejection.
	s.mu.Lock()
	delete(s.qrSessions, state)
	s.mu.Unlock()

	if err := s.CancelQRAuth(state, "intruder-1"); err != nil && !errors.Is(err, ErrQROwner) {
		t.Fatalf("wrong-owner cancellation: %v", err)
	}
	if owner, ok := st.pending[state]; !ok || owner != "owner-1" {
		t.Fatalf("wrong owner changed persisted pending row: owner=%q exists=%v", owner, ok)
	}

	if err := s.CancelQRAuth(state, "owner-1"); err != nil {
		t.Fatalf("owner cancellation after map loss: %v", err)
	}
	if _, ok := st.pending[state]; ok {
		t.Fatal("owner cancellation retained persisted pending row after map loss")
	}
}
