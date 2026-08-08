package authweb

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"time"
)

const (
	remoteCaptchaSecretBytes = 32
	remoteCaptchaMaxLifetime = 10 * time.Minute
)

var errRemoteCaptchaUnavailable = errors.New("remote captcha session unavailable")

type remoteCaptchaGrant struct {
	digest             [sha256.Size]byte
	ownerDiscordUserID string
	flow               *passwordFlow
	expiresAt          time.Time
	active             bool
}

type remoteCaptchaViewerSessionState struct {
	digest             [sha256.Size]byte
	ownerDiscordUserID string
	flow               *passwordFlow
	expiresAt          time.Time
	active             bool
	relay              *remoteCaptchaRelay
}

// remoteCaptchaViewer is the authenticated-session lookup seam consumed by
// the later WebSocket relay. It deliberately contains no bearer or cookie.
type remoteCaptchaViewer struct {
	state         string
	discordUserID string
	flow          *passwordFlow
	expiresAt     time.Time
}

type remoteCaptchaBindingCleanup struct {
	password   passwordStateCleanup
	staleFlows []*passwordFlow
}

type remoteCaptchaHooks struct {
	random io.Reader
	now    func() time.Time
	after  func(time.Duration) <-chan time.Time
}

func (s *Server) setRemoteCaptchaHooksForTest(hooks remoteCaptchaHooks) {
	s.remoteCaptchaRandom = hooks.random
	s.remoteCaptchaNow = hooks.now
	s.remoteCaptchaAfter = hooks.after
}

func (s *Server) clearRemoteCaptchaHooksForTest() {
	s.remoteCaptchaRandom = nil
	s.remoteCaptchaNow = nil
	s.remoteCaptchaAfter = nil
}

func (s *Server) remoteCaptchaHooks() remoteCaptchaHooks {
	hooks := remoteCaptchaHooks{
		random: s.remoteCaptchaRandom,
		now:    s.remoteCaptchaNow,
		after:  s.remoteCaptchaAfter,
	}
	if hooks.random == nil {
		hooks.random = rand.Reader
	}
	if hooks.now == nil {
		hooks.now = time.Now
	}
	if hooks.after == nil {
		hooks.after = time.After
	}
	return hooks
}

func newRemoteCaptchaSecret(random io.Reader) (raw string, digest [sha256.Size]byte, err error) {
	secret := make([]byte, remoteCaptchaSecretBytes)
	defer clear(secret)
	if _, err = io.ReadFull(random, secret); err != nil {
		return "", digest, err
	}
	digest = sha256.Sum256(secret)
	raw = base64.RawURLEncoding.EncodeToString(secret)
	return raw, digest, nil
}

func remoteCaptchaDigest(raw string) ([sha256.Size]byte, bool) {
	var zero [sha256.Size]byte
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(decoded) != remoteCaptchaSecretBytes ||
		base64.RawURLEncoding.EncodeToString(decoded) != raw {
		clear(decoded)
		return zero, false
	}
	digest := sha256.Sum256(decoded)
	clear(decoded)
	return digest, true
}

func remoteCaptchaDigestEqual(left, right [sha256.Size]byte) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func remoteCaptchaLifetime(pendingTTL time.Duration) time.Duration {
	if pendingTTL <= 0 || pendingTTL > remoteCaptchaMaxLifetime {
		return remoteCaptchaMaxLifetime
	}
	return pendingTTL
}

func (s *Server) redeemRemoteCaptchaGrant(rawGrant string) (remoteCaptchaViewer, string, error) {
	grantDigest, ok := remoteCaptchaDigest(rawGrant)
	if !ok {
		return remoteCaptchaViewer{}, "", errRemoteCaptchaUnavailable
	}
	hooks := s.remoteCaptchaHooks()
	rawSession, sessionDigest, err := newRemoteCaptchaSecret(hooks.random)
	if err != nil {
		return remoteCaptchaViewer{}, "", errRemoteCaptchaUnavailable
	}
	now := hooks.now()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return remoteCaptchaViewer{}, "", errRemoteCaptchaUnavailable
	}
	for state, pending := range s.passwordPending {
		grant := pending.remoteGrant
		outcome := s.passwordOutcomes[state]
		if !grant.active || !remoteCaptchaDigestEqual(grant.digest, grantDigest) {
			continue
		}
		if now.After(grant.expiresAt) || now.Equal(grant.expiresAt) || pending.flow == nil ||
			pending.flow != grant.flow || pending.discordUserID != grant.ownerDiscordUserID ||
			pending.flow.sealed || pending.flow.ctx.Err() != nil || outcome.done || pending.remoteViewer.active {
			cleanup := s.claimRemoteCaptchaBindingCleanupLocked(state, pending, grant.flow, pending.remoteViewer.flow)
			s.mu.Unlock()
			s.finishRemoteCaptchaBindingCleanup(cleanup)
			return remoteCaptchaViewer{}, "", errRemoteCaptchaUnavailable
		}

		pending.remoteGrant = remoteCaptchaGrant{}
		pending.remoteViewer = remoteCaptchaViewerSessionState{
			digest:             sessionDigest,
			ownerDiscordUserID: grant.ownerDiscordUserID,
			flow:               grant.flow,
			expiresAt:          grant.expiresAt,
			active:             true,
		}
		s.passwordPending[state] = pending
		viewer := remoteCaptchaViewer{
			state:         state,
			discordUserID: grant.ownerDiscordUserID,
			flow:          grant.flow,
			expiresAt:     grant.expiresAt,
		}
		s.mu.Unlock()
		return viewer, rawSession, nil
	}
	s.mu.Unlock()
	return remoteCaptchaViewer{}, "", errRemoteCaptchaUnavailable
}

func (s *Server) lookupRemoteCaptchaViewer(rawSession string) (remoteCaptchaViewer, error) {
	sessionDigest, ok := remoteCaptchaDigest(rawSession)
	if !ok {
		return remoteCaptchaViewer{}, errRemoteCaptchaUnavailable
	}
	now := s.remoteCaptchaHooks().now()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return remoteCaptchaViewer{}, errRemoteCaptchaUnavailable
	}
	for state, pending := range s.passwordPending {
		viewer := pending.remoteViewer
		outcome := s.passwordOutcomes[state]
		if !viewer.active || !remoteCaptchaDigestEqual(viewer.digest, sessionDigest) {
			continue
		}
		if !now.Before(viewer.expiresAt) || pending.flow == nil || pending.flow != viewer.flow ||
			pending.discordUserID != viewer.ownerDiscordUserID || pending.flow.sealed ||
			pending.flow.ctx.Err() != nil || outcome.done {
			cleanup := s.claimRemoteCaptchaBindingCleanupLocked(state, pending, pending.remoteGrant.flow, viewer.flow)
			s.mu.Unlock()
			s.finishRemoteCaptchaBindingCleanup(cleanup)
			return remoteCaptchaViewer{}, errRemoteCaptchaUnavailable
		}
		authenticated := remoteCaptchaViewer{
			state:         state,
			discordUserID: viewer.ownerDiscordUserID,
			flow:          viewer.flow,
			expiresAt:     viewer.expiresAt,
		}
		s.mu.Unlock()
		return authenticated, nil
	}
	s.mu.Unlock()
	return remoteCaptchaViewer{}, errRemoteCaptchaUnavailable
}

func (s *Server) cancelRemoteCaptchaViewer(rawSession string) error {
	sessionDigest, ok := remoteCaptchaDigest(rawSession)
	if !ok {
		return errRemoteCaptchaUnavailable
	}
	now := s.remoteCaptchaHooks().now()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errRemoteCaptchaUnavailable
	}
	for state, pending := range s.passwordPending {
		viewer := pending.remoteViewer
		outcome := s.passwordOutcomes[state]
		if !viewer.active || !remoteCaptchaDigestEqual(viewer.digest, sessionDigest) {
			continue
		}
		if !now.Before(viewer.expiresAt) || pending.flow == nil || pending.flow != viewer.flow ||
			pending.discordUserID != viewer.ownerDiscordUserID || pending.flow.sealed ||
			pending.flow.ctx.Err() != nil || outcome.done {
			cleanup := s.claimRemoteCaptchaBindingCleanupLocked(state, pending, pending.remoteGrant.flow, viewer.flow)
			s.mu.Unlock()
			s.finishRemoteCaptchaBindingCleanup(cleanup)
			return errRemoteCaptchaUnavailable
		}
		cleanup := s.claimPasswordStateCleanupLocked(state)
		s.mu.Unlock()
		if !cleanup.claimed {
			return errRemoteCaptchaUnavailable
		}
		s.finishPasswordStateCleanup(cleanup)
		return nil
	}
	s.mu.Unlock()
	return errRemoteCaptchaUnavailable
}

// claimRemoteCaptchaBindingCleanupLocked invalidates every credential and
// remote lifecycle involved in a binding mismatch before releasing Server.mu.
// Slow browser/flow cleanup is returned to the caller.
func (s *Server) claimRemoteCaptchaBindingCleanupLocked(state string, pending passwordPending, bindingFlows ...*passwordFlow) remoteCaptchaBindingCleanup {
	stale := make(map[*passwordFlow]struct{})
	for _, flow := range bindingFlows {
		if flow == nil {
			continue
		}
		releaseRemoteCaptchaFlow(flow)
		if flow != pending.flow {
			stale[flow] = struct{}{}
		}
	}
	scrubPasswordCredentials(&pending)
	clearRemoteCaptchaState(&pending)
	s.passwordPending[state] = pending
	cleanup := s.claimPasswordStateCleanupLocked(state)
	staleFlows := make([]*passwordFlow, 0, len(stale))
	for flow := range stale {
		staleFlows = append(staleFlows, flow)
	}
	return remoteCaptchaBindingCleanup{password: cleanup, staleFlows: staleFlows}
}

func (s *Server) finishRemoteCaptchaBindingCleanup(cleanup remoteCaptchaBindingCleanup) {
	s.finishPasswordStateCleanup(cleanup.password)
	for _, flow := range cleanup.staleFlows {
		flow.cancel()
		if closeErr := s.closeOwnedCaptchaBrowser(flow); closeErr != nil {
			log.Printf("stale CAPTCHA browser cleanup remains incomplete: %v", closeErr)
		}
		flow.wg.Wait()
	}
}

func clearRemoteCaptchaState(pending *passwordPending) {
	if pending == nil {
		return
	}
	if pending.remoteViewer.relay != nil {
		pending.remoteViewer.relay.cancel()
	}
	pending.remoteGrant = remoteCaptchaGrant{}
	pending.remoteViewer = remoteCaptchaViewerSessionState{}
	releaseRemoteCaptchaFlow(pending.flow)
}

func releaseRemoteCaptchaFlow(flow *passwordFlow) {
	if flow != nil && flow.remoteDone != nil {
		flow.remoteDoneOnce.Do(func() { close(flow.remoteDone) })
	}
}

func (s *Server) expireRemoteCaptchaState(state string, flow *passwordFlow, expiresAt time.Time, timer <-chan time.Time) {
	defer s.lifecycleWG.Done()
	for {
		select {
		case <-timer:
		case <-flow.ctx.Done():
			return
		case <-flow.remoteDone:
			return
		case <-s.lifecycleCtx.Done():
			return
		}

		hooks := s.remoteCaptchaHooks()
		if wait := expiresAt.Sub(hooks.now()); wait > 0 {
			timer = hooks.after(wait)
			continue
		}
		break
	}
	s.mu.Lock()
	pending, ok := s.passwordPending[state]
	if !ok || pending.flow != flow ||
		(pending.remoteGrant.expiresAt != expiresAt && pending.remoteViewer.expiresAt != expiresAt) {
		s.mu.Unlock()
		return
	}
	cleanup := s.claimPasswordStateCleanupLocked(state)
	s.mu.Unlock()
	s.finishPasswordStateCleanup(cleanup)
}
