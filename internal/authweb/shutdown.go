package authweb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const captchaShutdownTimeout = 2 * time.Second

// Shutdown seals the auth server, cancels in-flight auth work, stops the owned
// loopback CAPTCHA listener, reclaims owned browsers, and joins lifecycle
// workers. Multiple callers share the same shutdown operation.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.shutdownOnce.Do(func() {
		go s.shutdown()
	})
	select {
	case <-s.shutdownDone:
		return s.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close shuts the auth server down without a caller deadline.
func (s *Server) Close() error {
	return s.Shutdown(context.Background())
}

func (s *Server) shutdown() {
	defer close(s.shutdownDone)

	var (
		passwordCleanups []passwordStateCleanup
		commitStates     []string
		mfaFlows         []*mfaFlow
		shutdownErr      error
		tlsServer        *http.Server
		tlsListener      net.Listener
		tlsDone          <-chan struct{}
	)

	s.mu.Lock()
	s.closed = true
	for state, ready := range s.passwordReady {
		if ready != nil {
			close(ready)
		}
		delete(s.passwordReady, state)
	}
	for state, pending := range s.passwordPending {
		if pending.flow != nil && pending.flow.commitClaimed {
			pending.flow.cleanupRequested = true
			commitStates = append(commitStates, state)
			continue
		}
		if cleanup := s.claimPasswordStateCleanupLocked(state); cleanup.claimed {
			passwordCleanups = append(passwordCleanups, cleanup)
		}
	}
	for state, pending := range s.mfaPending {
		delete(s.mfaPending, state)
		if pending.flow != nil {
			mfaFlows = append(mfaFlows, pending.flow)
		}
	}
	clear(s.qrSessions)
	tlsServer = s.captchaTLSServer
	tlsListener = s.captchaTLSListener
	tlsDone = s.captchaTLSDone
	afterClosed := s.afterShutdownClosedForTest
	s.mu.Unlock()
	if afterClosed != nil {
		afterClosed()
	}

	// Cancel contexts before waiting. None of the calls below run while
	// Server.mu is held.
	s.lifecycleCancel()
	for _, flow := range mfaFlows {
		flow.cancel()
	}
	for _, cleanup := range passwordCleanups {
		s.finishPasswordStateCleanup(cleanup)
	}
	for _, flow := range mfaFlows {
		flow.wg.Wait()
	}
	for _, state := range commitStates {
		s.mu.Lock()
		pending, ok := s.passwordPending[state]
		s.mu.Unlock()
		if ok && pending.flow != nil {
			pending.flow.wg.Wait()
		}
		s.cleanupPasswordState(state)
	}

	if tlsServer != nil {
		tlsCtx, cancel := context.WithTimeout(context.Background(), captchaShutdownTimeout)
		err := tlsServer.Shutdown(tlsCtx)
		cancel()
		if err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("stop CAPTCHA TLS: %w", err))
			// Shutdown leaves active connections open when its context expires.
			// Close the server itself so partial requests and other accepted
			// connections cannot outlive the auth server.
			if closeErr := tlsServer.Close(); closeErr != nil &&
				!errors.Is(closeErr, net.ErrClosed) && !errors.Is(closeErr, http.ErrServerClosed) {
				shutdownErr = errors.Join(shutdownErr, fmt.Errorf("force-close CAPTCHA TLS: %w", closeErr))
			}
		}
	} else if tlsListener != nil {
		if err := tlsListener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("close CAPTCHA listener: %w", err))
		}
	}
	if tlsDone != nil {
		select {
		case <-tlsDone:
		case <-time.After(captchaShutdownTimeout):
			shutdownErr = errors.Join(shutdownErr, errors.New("CAPTCHA TLS did not stop before deadline"))
		}
	}

	s.lifecycleWG.Wait()

	// Lifecycle enrollment and the closed publication gate make this the final
	// stable set of legacy browser-auth states. Consume persisted pending rows
	// outside Server.mu, then leave no in-memory outcome behind.
	s.mu.Lock()
	legacyStates := make([]string, 0, len(s.outcomes))
	for state := range s.outcomes {
		legacyStates = append(legacyStates, state)
	}
	clear(s.outcomes)
	s.mu.Unlock()
	for _, state := range legacyStates {
		if _, _, err := s.store.TakeAuthPending(state); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("rollback pending browser auth: %w", err))
		}
	}

	if err := s.retryRetainedCaptchaBrowsers(3); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
	}

	s.mu.Lock()
	s.captchaTLSServer = nil
	s.captchaTLSListener = nil
	s.captchaTLSPort = 0
	s.captchaTLSDone = nil
	s.shutdownErr = shutdownErr
	s.mu.Unlock()
}

func (s *Server) retryRetainedCaptchaBrowsers(attempts int) error {
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		closeAttempts := s.retainedCaptchaBrowserCloseAttempts()
		if len(closeAttempts) == 0 {
			return nil
		}
		for _, closeAttempt := range closeAttempts {
			_ = s.closeRetainedCaptchaBrowser(closeAttempt)
		}
	}
	s.mu.Lock()
	remaining := len(s.captchaCloseFailures)
	s.mu.Unlock()
	if remaining != 0 {
		return fmt.Errorf("%d CAPTCHA browser cleanup operation(s) remain incomplete", remaining)
	}
	return nil
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	return closed
}

func (s *Server) beginLifecycleOperation(parent context.Context) (context.Context, func(), error) {
	if parent == nil {
		parent = context.Background()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, ErrServerClosed
	}
	s.lifecycleWG.Add(1)
	root := s.lifecycleCtx
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(root, cancel)
	return ctx, func() {
		stop()
		cancel()
		s.lifecycleWG.Done()
	}, nil
}
