package authweb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
)

// Mutation caught: deleting before checking the Discord owner, or rejecting an
// expired-but-present owner cleanup, either leaks or lets an intruder erase MFA.
func TestCancelPasswordMFAIsOwnerBoundIdempotentAndExpiredSafe(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	flowCtx, flowCancel := context.WithCancel(context.Background())
	flow := &mfaFlow{ctx: flowCtx, cancel: flowCancel}
	s.mu.Lock()
	s.mfaPending["expired-mfa"] = mfaPending{
		discordUserID: "owner-1",
		challenge:     &riot.MFAChallenge{Method: "email"},
		flow:          flow,
		expiresAt:     time.Now().Add(-time.Minute),
	}
	s.mu.Unlock()

	if err := s.CancelPasswordMFA("expired-mfa", "intruder-1"); !errors.Is(err, ErrMFAOwner) {
		t.Fatalf("wrong-owner cancellation error=%v, want ErrMFAOwner", err)
	}
	s.mu.Lock()
	_, retained := s.mfaPending["expired-mfa"]
	s.mu.Unlock()
	if !retained || flow.ctx.Err() != nil {
		t.Fatal("wrong-owner cancellation removed or canceled the MFA continuation")
	}

	if err := s.CancelPasswordMFA("expired-mfa", "owner-1"); err != nil {
		t.Fatalf("owner cancellation of expired state: %v", err)
	}
	s.mu.Lock()
	_, retained = s.mfaPending["expired-mfa"]
	s.mu.Unlock()
	if retained || !errors.Is(flow.ctx.Err(), context.Canceled) {
		t.Fatalf("owner cancellation retained state=%v flowErr=%v", retained, flow.ctx.Err())
	}
	if err := s.CancelPasswordMFA("expired-mfa", "owner-1"); err != nil {
		t.Fatalf("idempotent cancellation error=%v", err)
	}
}

// Mutation caught: waiting on an MFA flow before releasing Server.mu deadlocks
// a canceled worker that needs the mutex, while omitting the wait returns early.
func TestCancelPasswordMFADetachesCancelsAndWaitsOutsideServerMutex(t *testing.T) {
	s := newCaptchaServer(&fakePasswordAuth{})
	flowCtx, flowCancel := context.WithCancel(context.Background())
	flow := &mfaFlow{ctx: flowCtx, cancel: flowCancel}
	flow.wg.Add(1)
	workerObservedDetach := make(chan bool, 1)
	workerRelease := make(chan struct{})
	go func() {
		defer flow.wg.Done()
		<-flow.ctx.Done()
		s.mu.Lock()
		_, stillPending := s.mfaPending["live-mfa"]
		s.mu.Unlock()
		workerObservedDetach <- !stillPending
		<-workerRelease
	}()
	s.mu.Lock()
	s.mfaPending["live-mfa"] = mfaPending{
		discordUserID: "owner-1",
		challenge:     &riot.MFAChallenge{Method: "email"},
		flow:          flow,
		expiresAt:     time.Now().Add(time.Hour),
	}
	s.mu.Unlock()

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- s.CancelPasswordMFA("live-mfa", "owner-1") }()
	select {
	case detached := <-workerObservedDetach:
		if !detached {
			close(workerRelease)
			t.Fatal("MFA worker observed cancellation before atomic detachment")
		}
	case <-time.After(200 * time.Millisecond):
		close(workerRelease)
		t.Fatal("canceled MFA worker could not acquire Server.mu")
	}
	select {
	case err := <-cancelDone:
		close(workerRelease)
		t.Fatalf("CancelPasswordMFA returned before in-flight work drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(workerRelease)
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("CancelPasswordMFA did not return after worker drain")
	}
}
