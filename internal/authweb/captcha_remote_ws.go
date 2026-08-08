package authweb

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	remoteCaptchaWebSocketReadBufferSize   = 4 << 10
	remoteCaptchaWebSocketWriteBufferSize  = 32 << 10
	remoteCaptchaWebSocketWriteWait        = 5 * time.Second
	remoteCaptchaWebSocketPongWait         = 30 * time.Second
	remoteCaptchaWebSocketPingPeriod       = 25 * time.Second
	remoteCaptchaDisconnectGrace           = time.Minute
	remoteCaptchaStreamAttachRetryInterval = 25 * time.Millisecond
	remoteCaptchaStreamAttachTimeout       = 5 * time.Second
)

const remoteCaptchaStreamSessionUnavailable = "remote CAPTCHA Chrome session is unavailable"

var errRemoteCaptchaViewerConflict = errors.New("remote captcha viewer already connected")

type remoteCaptchaStreamSession interface {
	Frames() <-chan []byte
	DispatchInput(context.Context, []byte) error
	Done() <-chan struct{}
	Err() error
	Close(context.Context) error
}

type remoteCaptchaStreamStartFunc func(captchaBrowserController, context.Context, context.Context, context.Context, context.Context) (remoteCaptchaStreamSession, error)
type remoteCaptchaProcessDoneFunc func(captchaBrowserController) <-chan struct{}

type remoteCaptchaDisconnectTimer interface {
	Chan() <-chan time.Time
	Stop() bool
}

type systemRemoteCaptchaDisconnectTimer struct {
	*time.Timer
}

func (t *systemRemoteCaptchaDisconnectTimer) Chan() <-chan time.Time { return t.C }

func newRemoteCaptchaDisconnectTimer(duration time.Duration) remoteCaptchaDisconnectTimer {
	return &systemRemoteCaptchaDisconnectTimer{Timer: time.NewTimer(duration)}
}

type remoteCaptchaWebSocketWriter interface {
	SetWriteDeadline(time.Time) error
	WriteMessage(int, []byte) error
	WriteControl(int, []byte, time.Time) error
}

type remoteCaptchaWebSocketConnection interface {
	remoteCaptchaWebSocketWriter
	SetReadLimit(int64)
	SetReadDeadline(time.Time) error
	SetPongHandler(func(string) error)
	ReadMessage() (int, []byte, error)
	Close() error
}

type remoteCaptchaWebSocketTiming struct {
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
}

func defaultRemoteCaptchaWebSocketTiming() remoteCaptchaWebSocketTiming {
	return remoteCaptchaWebSocketTiming{now: time.Now, after: time.After}
}

type remoteCaptchaRelay struct {
	state         string
	flow          *passwordFlow
	sessionDigest [sha256.Size]byte
	expiresAt     time.Time
	ctx           context.Context
	cancel        context.CancelFunc
	start         chan struct{}
	startOnce     sync.Once
	ready         chan struct{}
	readyOnce     sync.Once
	stream        remoteCaptchaStreamSession
	startErr      error

	// Protected by Server.mu.
	connectionActive     bool
	connectionGeneration uint64
	graceGeneration      uint64
	graceCancel          context.CancelFunc
	graceDone            chan struct{}
}

type remoteCaptchaWebSocketClaim struct {
	relay      *remoteCaptchaRelay
	generation uint64
}

func defaultRemoteCaptchaStartStream(controller captchaBrowserController, flowCtx, processCtx, viewerCtx, serverCtx context.Context) (remoteCaptchaStreamSession, error) {
	starter, ok := controller.(interface {
		StartRemoteCaptchaStream(context.Context, context.Context, context.Context, context.Context) (*remoteCaptchaStream, error)
	})
	if !ok {
		return nil, errors.New("captcha Chrome controller cannot stream the remote viewer")
	}
	return starter.StartRemoteCaptchaStream(flowCtx, processCtx, viewerCtx, serverCtx)
}

func defaultRemoteCaptchaProcessDone(controller captchaBrowserController) <-chan struct{} {
	chrome, ok := controller.(*chromeBrowserController)
	if !ok || chrome == nil {
		return nil
	}
	return chrome.exited
}

func (s *Server) handleRemoteCaptchaWebSocket(w http.ResponseWriter, r *http.Request) {
	setRemoteCaptchaSecurityHeaders(w)
	if !s.allowRemoteCaptchaRequest(r, true) {
		writeRemoteCaptchaError(w, http.StatusForbidden)
		return
	}
	cookie, err := r.Cookie(remoteCaptchaViewerCookieName)
	if err != nil {
		writeRemoteCaptchaError(w, http.StatusUnauthorized)
		return
	}
	claim, err := s.claimRemoteCaptchaWebSocket(cookie.Value)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, errRemoteCaptchaViewerConflict) {
			status = http.StatusConflict
		}
		writeRemoteCaptchaError(w, status)
		return
	}
	unexpectedDisconnect := true
	defer func() { s.releaseRemoteCaptchaWebSocket(claim, unexpectedDisconnect) }()

	upgrader := websocket.Upgrader{
		ReadBufferSize:  remoteCaptchaWebSocketReadBufferSize,
		WriteBufferSize: remoteCaptchaWebSocketWriteBufferSize,
		CheckOrigin: func(request *http.Request) bool {
			return s.allowRemoteCaptchaRequest(request, true)
		},
	}
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	if !s.activateRemoteCaptchaWebSocket(claim) {
		unexpectedDisconnect = false
		return
	}
	select {
	case <-claim.relay.ready:
	case <-claim.relay.ctx.Done():
		unexpectedDisconnect = false
		return
	case <-s.lifecycleCtx.Done():
		unexpectedDisconnect = false
		return
	}
	if claim.relay.stream == nil || claim.relay.startErr != nil {
		unexpectedDisconnect = false
		return
	}
	if s.serveRemoteCaptchaWebSocket(connection, claim.relay, s.remoteCaptchaWebSocketTiming) {
		unexpectedDisconnect = false
		s.cleanupPasswordState(claim.relay.state)
		return
	}
	if claim.relay.ctx.Err() != nil || s.lifecycleCtx.Err() != nil {
		unexpectedDisconnect = false
	}
}

func (s *Server) serveRemoteCaptchaWebSocket(connection remoteCaptchaWebSocketConnection, relay *remoteCaptchaRelay, timing remoteCaptchaWebSocketTiming) (protocolViolation bool) {
	if timing.now == nil {
		timing.now = time.Now
	}
	if timing.after == nil {
		timing.after = time.After
	}
	connection.SetReadLimit(remoteCaptchaMaxInputPayloadSize)
	if err := connection.SetReadDeadline(timing.now().Add(remoteCaptchaWebSocketPongWait)); err != nil {
		return false
	}
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(timing.now().Add(remoteCaptchaWebSocketPongWait))
	})

	writerCtx, stopWriter := context.WithCancel(relay.ctx)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- writeRemoteCaptchaWebSocket(writerCtx, connection, relay.stream, timing)
		_ = connection.Close()
	}()

	for {
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			protocolViolation = remoteCaptchaProtocolReadViolation(err)
			break
		}
		if messageType != websocket.TextMessage {
			protocolViolation = true
			break
		}
		inputCtx, cancel := context.WithTimeout(relay.ctx, remoteCaptchaCommandTimeout)
		err = relay.stream.DispatchInput(inputCtx, payload)
		cancel()
		if err != nil {
			if errors.Is(err, errRemoteCaptchaInputRate) || errors.Is(err, errRemoteCaptchaInputBusy) {
				continue
			}
			protocolViolation = true
			break
		}
	}
	stopWriter()
	_ = connection.Close()
	<-writerDone
	return protocolViolation
}

func remoteCaptchaProtocolReadViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, websocket.ErrReadLimit) {
		return true
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseProtocolError,
			websocket.CloseUnsupportedData,
			websocket.CloseInvalidFramePayloadData,
			websocket.ClosePolicyViolation,
			websocket.CloseMessageTooBig:
			return true
		default:
			return false
		}
	}
	// Gorilla reports malformed frame headers as package-prefixed plain
	// errors after it has sent a protocol close frame. Transport failures are
	// typed network/close errors and do not take this path.
	return strings.HasPrefix(err.Error(), "websocket:")
}

func writeRemoteCaptchaWebSocket(ctx context.Context, connection remoteCaptchaWebSocketWriter, stream remoteCaptchaStreamSession, timing remoteCaptchaWebSocketTiming) error {
	if timing.now == nil {
		timing.now = time.Now
	}
	if timing.after == nil {
		timing.after = time.After
	}
	ping := timing.after(remoteCaptchaWebSocketPingPeriod)
	for {
		select {
		case <-ctx.Done():
			deadline := timing.now().Add(remoteCaptchaWebSocketWriteWait)
			_ = connection.SetWriteDeadline(deadline)
			return connection.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				deadline)
		case frame, ok := <-stream.Frames():
			if !ok {
				return nil
			}
			if err := connection.SetWriteDeadline(timing.now().Add(remoteCaptchaWebSocketWriteWait)); err != nil {
				return err
			}
			if err := connection.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return err
			}
		case <-ping:
			deadline := timing.now().Add(remoteCaptchaWebSocketWriteWait)
			if err := connection.SetWriteDeadline(deadline); err != nil {
				return err
			}
			if err := connection.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				return err
			}
			ping = timing.after(remoteCaptchaWebSocketPingPeriod)
		}
	}
}

func (s *Server) claimRemoteCaptchaWebSocket(rawSession string) (remoteCaptchaWebSocketClaim, error) {
	sessionDigest, ok := remoteCaptchaDigest(rawSession)
	if !ok {
		return remoteCaptchaWebSocketClaim{}, errRemoteCaptchaUnavailable
	}
	now := s.remoteCaptchaHooks().now()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return remoteCaptchaWebSocketClaim{}, errRemoteCaptchaUnavailable
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
			return remoteCaptchaWebSocketClaim{}, errRemoteCaptchaUnavailable
		}
		relay := viewer.relay
		if relay == nil {
			relayCtx, relayCancel := context.WithCancel(context.Background())
			relay = &remoteCaptchaRelay{
				state:         state,
				flow:          pending.flow,
				sessionDigest: sessionDigest,
				expiresAt:     viewer.expiresAt,
				ctx:           relayCtx,
				cancel:        relayCancel,
				start:         make(chan struct{}),
				ready:         make(chan struct{}),
			}
			s.lifecycleWG.Add(1)
			pending.flow.wg.Add(1)
			viewer.relay = relay
			pending.remoteViewer = viewer
			s.passwordPending[state] = pending
			go s.runRemoteCaptchaRelay(relay)
		} else if relay.flow != pending.flow || relay.state != state ||
			!remoteCaptchaDigestEqual(relay.sessionDigest, sessionDigest) {
			cleanup := s.claimRemoteCaptchaBindingCleanupLocked(state, pending, pending.remoteGrant.flow, viewer.flow, relay.flow)
			s.mu.Unlock()
			s.finishRemoteCaptchaBindingCleanup(cleanup)
			return remoteCaptchaWebSocketClaim{}, errRemoteCaptchaUnavailable
		}
		if relay.connectionActive {
			s.mu.Unlock()
			return remoteCaptchaWebSocketClaim{}, errRemoteCaptchaViewerConflict
		}
		s.lifecycleWG.Add(1)
		relay.connectionActive = true
		relay.connectionGeneration++
		relay.graceGeneration++
		graceCancel := relay.graceCancel
		graceDone := relay.graceDone
		relay.graceCancel = nil
		relay.graceDone = nil
		claim := remoteCaptchaWebSocketClaim{relay: relay, generation: relay.connectionGeneration}
		s.mu.Unlock()
		if graceCancel != nil {
			graceCancel()
			<-graceDone
		}
		return claim, nil
	}
	s.mu.Unlock()
	return remoteCaptchaWebSocketClaim{}, errRemoteCaptchaUnavailable
}

func (s *Server) activateRemoteCaptchaWebSocket(claim remoteCaptchaWebSocketClaim) bool {
	if claim.relay == nil {
		return false
	}
	s.mu.Lock()
	pending, ok := s.passwordPending[claim.relay.state]
	live := !s.closed && ok && pending.flow == claim.relay.flow && pending.remoteViewer.relay == claim.relay &&
		claim.relay.connectionActive && claim.relay.connectionGeneration == claim.generation &&
		claim.relay.ctx.Err() == nil
	s.mu.Unlock()
	if live {
		claim.relay.startOnce.Do(func() { close(claim.relay.start) })
	}
	return live
}

func (s *Server) releaseRemoteCaptchaWebSocket(claim remoteCaptchaWebSocketClaim, unexpected bool) {
	if claim.relay != nil {
		s.mu.Lock()
		if claim.relay.connectionActive && claim.relay.connectionGeneration == claim.generation {
			claim.relay.connectionActive = false
			pending, live := s.passwordPending[claim.relay.state]
			outcome := s.passwordOutcomes[claim.relay.state]
			if unexpected && !s.closed && live && pending.flow == claim.relay.flow &&
				pending.remoteViewer.relay == claim.relay && !pending.flow.sealed &&
				pending.flow.ctx.Err() == nil && !outcome.done && claim.relay.ctx.Err() == nil &&
				s.remoteCaptchaHooks().now().Before(claim.relay.expiresAt) &&
				claim.relay.graceCancel == nil && claim.relay.graceDone == nil {
				claim.relay.graceGeneration++
				generation := claim.relay.graceGeneration
				timerFactory := s.remoteCaptchaGraceTimer
				if timerFactory == nil {
					timerFactory = newRemoteCaptchaDisconnectTimer
				}
				timer := timerFactory(remoteCaptchaDisconnectGrace)
				graceCtx, cancelGraceContext := context.WithCancel(claim.relay.ctx)
				var stopGraceOnce sync.Once
				graceCancel := func() {
					stopGraceOnce.Do(func() {
						_ = timer.Stop()
						cancelGraceContext()
					})
				}
				graceDone := make(chan struct{})
				s.lifecycleWG.Add(1)
				claim.relay.graceCancel = graceCancel
				claim.relay.graceDone = graceDone
				go s.expireRemoteCaptchaDisconnectGrace(claim.relay, generation, timer.Chan(), graceCtx, graceCancel, graceDone)
			}
		}
		s.mu.Unlock()
	}
	s.lifecycleWG.Done()
}

func (s *Server) expireRemoteCaptchaDisconnectGrace(relay *remoteCaptchaRelay, generation uint64, timer <-chan time.Time, graceCtx context.Context, cancelGrace context.CancelFunc, done chan<- struct{}) {
	defer func() {
		cancelGrace()
		if hook := s.beforeRemoteCaptchaGraceDoneForTest; hook != nil {
			hook()
		}
		close(done)
		s.lifecycleWG.Done()
	}()
	select {
	case <-timer:
	case <-graceCtx.Done():
		return
	case <-s.lifecycleCtx.Done():
		return
	}
	if hook := s.afterRemoteCaptchaGraceTimerForTest; hook != nil {
		hook()
	}
	s.mu.Lock()
	pending, ok := s.passwordPending[relay.state]
	if !ok || pending.flow != relay.flow || pending.remoteViewer.relay != relay ||
		relay.connectionActive || relay.graceGeneration != generation || relay.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	cleanup := s.claimPasswordStateCleanupLocked(relay.state)
	s.mu.Unlock()
	s.finishPasswordStateCleanup(cleanup)
}

func (s *Server) runRemoteCaptchaRelay(relay *remoteCaptchaRelay) {
	defer s.lifecycleWG.Done()
	defer relay.flow.wg.Done()
	select {
	case <-relay.start:
	case <-relay.ctx.Done():
		relay.publishReady(nil, relay.ctx.Err())
		return
	case <-s.lifecycleCtx.Done():
		relay.publishReady(nil, s.lifecycleCtx.Err())
		return
	}
	if err := s.ensureCaptchaLaunched(relay.state); err != nil {
		relay.publishReady(nil, err)
		s.failRemoteCaptchaRelay(relay, fmt.Errorf("launch remote CAPTCHA browser: %w", err))
		return
	}

	relay.flow.launchMu.Lock()
	controller := relay.flow.browser
	relay.flow.launchMu.Unlock()
	if controller == nil {
		err := errors.New("remote CAPTCHA browser exited before streaming")
		relay.publishReady(nil, err)
		s.failRemoteCaptchaRelay(relay, err)
		return
	}
	processDone := s.remoteCaptchaProcessDone(controller)
	if processDone == nil {
		err := errors.New("remote CAPTCHA browser process lifetime unavailable")
		relay.publishReady(nil, err)
		s.failRemoteCaptchaRelay(relay, err)
		return
	}
	processCtx, processCancel := context.WithCancel(context.Background())
	processWatchDone := make(chan struct{})
	go func() {
		defer close(processWatchDone)
		select {
		case <-processDone:
			processCancel()
		case <-relay.ctx.Done():
		case <-s.lifecycleCtx.Done():
		}
	}()
	defer func() {
		processCancel()
		<-processWatchDone
	}()

	stream, err := s.startRemoteCaptchaStreamWhenReady(controller, relay, processCtx)
	if err != nil {
		relay.publishReady(nil, err)
		s.failRemoteCaptchaRelay(relay, fmt.Errorf("start remote CAPTCHA stream: %w", err))
		return
	}
	relay.publishReady(stream, nil)

	select {
	case <-relay.ctx.Done():
		closeRemoteCaptchaStream(stream)
		return
	case <-s.lifecycleCtx.Done():
		closeRemoteCaptchaStream(stream)
		return
	case <-processCtx.Done():
		closeRemoteCaptchaStream(stream)
		if relay.ctx.Err() == nil && s.lifecycleCtx.Err() == nil {
			s.failRemoteCaptchaRelay(relay, errors.New("remote CAPTCHA browser exited"))
		}
		return
	case <-stream.Done():
		if relay.ctx.Err() == nil && processCtx.Err() == nil && s.lifecycleCtx.Err() == nil {
			err := stream.Err()
			if err == nil {
				err = errors.New("remote CAPTCHA stream ended unexpectedly")
			}
			s.failRemoteCaptchaRelay(relay, err)
		}
	}
}

func (s *Server) startRemoteCaptchaStreamWhenReady(controller captchaBrowserController, relay *remoteCaptchaRelay, processCtx context.Context) (remoteCaptchaStreamSession, error) {
	startCtx, cancel := context.WithTimeout(relay.ctx, remoteCaptchaStreamAttachTimeout)
	defer cancel()
	for {
		stream, err := s.remoteCaptchaStartStream(controller, relay.flow.ctx, processCtx, relay.ctx, s.lifecycleCtx)
		if err == nil || err.Error() != remoteCaptchaStreamSessionUnavailable {
			return stream, err
		}
		after := s.remoteCaptchaStreamRetryAfter
		if after == nil {
			after = time.After
		}
		select {
		case <-after(remoteCaptchaStreamAttachRetryInterval):
		case <-startCtx.Done():
			return nil, startCtx.Err()
		case <-processCtx.Done():
			return nil, errors.New("remote CAPTCHA browser exited before its page session was ready")
		case <-s.lifecycleCtx.Done():
			return nil, s.lifecycleCtx.Err()
		}
	}
}

func (relay *remoteCaptchaRelay) publishReady(stream remoteCaptchaStreamSession, err error) {
	relay.readyOnce.Do(func() {
		relay.stream = stream
		relay.startErr = err
		close(relay.ready)
	})
}

func closeRemoteCaptchaStream(stream remoteCaptchaStreamSession) {
	if stream == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteCaptchaCommandTimeout)
	_ = stream.Close(ctx)
	cancel()
}

func (s *Server) failRemoteCaptchaRelay(relay *remoteCaptchaRelay, err error) {
	if relay == nil || err == nil {
		return
	}
	relay.cancel()
	_, _ = s.setPasswordOutcome(relay.state, relay.flow, passwordOutcome{err: err})
}
