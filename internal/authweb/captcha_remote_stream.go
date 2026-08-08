package authweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"
)

const (
	remoteCaptchaViewportWidth        = 1280
	remoteCaptchaViewportHeight       = 900
	remoteCaptchaJPEGQuality          = 80
	remoteCaptchaCommandTimeout       = time.Second
	remoteCaptchaMaxDecodedFrameSize  = 2 << 20
	remoteCaptchaMaxInputPayloadSize  = 4 << 10
	remoteCaptchaMaxWheelDelta        = 900
	remoteCaptchaInputEventsPerSecond = 60
	remoteCaptchaInputBurst           = 8
	remoteCaptchaMaxOutstandingInput  = 4
)

var (
	errRemoteCaptchaFrameInvalid     = errors.New("remote CAPTCHA frame is invalid")
	errRemoteCaptchaFrameTooLarge    = errors.New("remote CAPTCHA frame exceeds 2 MiB")
	errRemoteCaptchaInputInvalid     = errors.New("remote CAPTCHA input is invalid")
	errRemoteCaptchaInputRate        = errors.New("remote CAPTCHA input rate exceeded")
	errRemoteCaptchaInputBusy        = errors.New("remote CAPTCHA input queue is full")
	errRemoteCaptchaLifetimeRequired = errors.New("remote CAPTCHA requires all owner lifetimes")
)

type remoteCaptchaScreencastFrame struct {
	Data      json.RawMessage `json:"data"`
	SessionID int64           `json:"sessionId"`
}

type remoteCaptchaInputMessage struct {
	Type   string   `json:"type"`
	Phase  string   `json:"phase,omitempty"`
	X      *float64 `json:"x,omitempty"`
	Y      *float64 `json:"y,omitempty"`
	Width  *float64 `json:"width,omitempty"`
	Height *float64 `json:"height,omitempty"`
	Button *int     `json:"button,omitempty"`
	DeltaY *float64 `json:"deltaY,omitempty"`
}

type remoteCaptchaMouseEvent struct {
	Type        string  `json:"type"`
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	Button      string  `json:"button"`
	Buttons     int     `json:"buttons"`
	ClickCount  int     `json:"clickCount,omitempty"`
	DeltaX      float64 `json:"deltaX,omitempty"`
	DeltaY      float64 `json:"deltaY,omitempty"`
	PointerType string  `json:"pointerType"`
}

type remoteCaptchaInputLimiter struct {
	mu     sync.Mutex
	now    func() time.Time
	last   time.Time
	tokens float64
}

type remoteCaptchaStream struct {
	client         *chromeDevToolsClient
	subscription   *chromeDevToolsEventSubscription
	ctx            context.Context
	cancel         context.CancelFunc
	parentStops    []func() bool
	done           chan struct{}
	frames         chan []byte
	inputLimiter   *remoteCaptchaInputLimiter
	inputSlots     chan struct{}
	commandTimeout time.Duration

	mu             sync.Mutex
	terminal       error
	stopErr        error
	closeRequested bool
	closeOnce      sync.Once
	finishOnce     sync.Once
}

func (c *chromeBrowserController) StartRemoteCaptchaStream(flowCtx, chromeCtx, viewerCtx, shutdownCtx context.Context) (*remoteCaptchaStream, error) {
	client, err := c.chromeDevToolsClient()
	if err != nil {
		return nil, err
	}
	return newRemoteCaptchaStream(client, client.currentSessionID(), flowCtx, chromeCtx, viewerCtx, shutdownCtx)
}

func newRemoteCaptchaStream(client *chromeDevToolsClient, sessionID string, flowCtx, chromeCtx, viewerCtx, shutdownCtx context.Context) (*remoteCaptchaStream, error) {
	if client == nil {
		return nil, errChromeDevToolsClientClosed
	}
	if flowCtx == nil || chromeCtx == nil || viewerCtx == nil || shutdownCtx == nil {
		return nil, errRemoteCaptchaLifetimeRequired
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || sessionID != client.currentSessionID() {
		return nil, errors.New("remote CAPTCHA Chrome session is unavailable")
	}
	ctx, cancel, parentStops := remoteCaptchaLifetimeContext(flowCtx, chromeCtx, viewerCtx, shutdownCtx)
	if err := ctx.Err(); err != nil {
		cancel()
		stopRemoteCaptchaParentWatches(parentStops)
		return nil, err
	}
	subscription, err := client.SubscribeEvents(sessionID, "Page.screencastFrame")
	if err != nil {
		cancel()
		stopRemoteCaptchaParentWatches(parentStops)
		return nil, fmt.Errorf("subscribe remote CAPTCHA frames: %w", err)
	}
	if err := client.Call(ctx, "Page.startScreencast", map[string]any{
		"format":        "jpeg",
		"quality":       remoteCaptchaJPEGQuality,
		"maxWidth":      remoteCaptchaViewportWidth,
		"maxHeight":     remoteCaptchaViewportHeight,
		"everyNthFrame": 1,
	}, nil); err != nil {
		subscription.Close()
		cancel()
		stopRemoteCaptchaParentWatches(parentStops)
		return nil, fmt.Errorf("start remote CAPTCHA screencast: %w", err)
	}
	stream := &remoteCaptchaStream{
		client:         client,
		subscription:   subscription,
		ctx:            ctx,
		cancel:         cancel,
		parentStops:    parentStops,
		done:           make(chan struct{}),
		frames:         make(chan []byte, 1),
		inputLimiter:   newRemoteCaptchaInputLimiter(time.Now),
		inputSlots:     make(chan struct{}, remoteCaptchaMaxOutstandingInput),
		commandTimeout: remoteCaptchaCommandTimeout,
	}
	go stream.run()
	return stream, nil
}

func remoteCaptchaLifetimeContext(flowCtx, chromeCtx, viewerCtx, shutdownCtx context.Context) (context.Context, context.CancelFunc, []func() bool) {
	ctx, cancel := context.WithCancel(context.Background())
	parents := [4]context.Context{flowCtx, chromeCtx, viewerCtx, shutdownCtx}
	stops := make([]func() bool, 0, len(parents))
	for _, parent := range parents {
		if parent.Err() != nil {
			cancel()
			continue
		}
		stops = append(stops, context.AfterFunc(parent, cancel))
	}
	return ctx, cancel, stops
}

func stopRemoteCaptchaParentWatches(stops []func() bool) {
	for _, stop := range stops {
		stop()
	}
}

func (s *remoteCaptchaStream) run() {
	terminal := error(nil)
	for terminal == nil {
		var event chromeDevToolsMessage
		event, terminal = s.subscription.Next(s.ctx)
		if terminal != nil {
			break
		}
		terminal = s.handleScreencastFrame(event)
	}
	commandTimeout := s.commandTimeout
	if commandTimeout <= 0 {
		commandTimeout = remoteCaptchaCommandTimeout
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), commandTimeout)
	stopErr := s.client.Call(stopCtx, "Page.stopScreencast", map[string]any{}, nil)
	stopCancel()
	s.finish(terminal, stopErr)
}

func (s *remoteCaptchaStream) finish(terminal, stopErr error) {
	s.finishOnce.Do(func() {
		s.subscription.Close()
		stopRemoteCaptchaParentWatches(s.parentStops)
		s.cancel()
		s.mu.Lock()
		s.stopErr = stopErr
		s.terminal = errors.Join(terminal, stopErr)
		s.mu.Unlock()
		close(s.frames)
		close(s.done)
	})
}

func (s *remoteCaptchaStream) handleScreencastFrame(event chromeDevToolsMessage) error {
	var params remoteCaptchaScreencastFrame
	if err := json.Unmarshal(event.Params, &params); err != nil || params.SessionID <= 0 {
		return errRemoteCaptchaFrameInvalid
	}
	if err := s.client.Call(s.ctx, "Page.screencastFrameAck", map[string]any{
		"sessionId": params.SessionID,
	}, nil); err != nil {
		return fmt.Errorf("acknowledge remote CAPTCHA frame: %w", err)
	}
	var encoded string
	if err := json.Unmarshal(params.Data, &encoded); err != nil || encoded == "" {
		return errRemoteCaptchaFrameInvalid
	}
	frame, err := decodeRemoteCaptchaFrame(encoded)
	if err != nil {
		return err
	}
	select {
	case s.frames <- frame:
		return nil
	default:
	}
	select {
	case <-s.frames:
	default:
	}
	select {
	case s.frames <- frame:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func decodeRemoteCaptchaFrame(encoded string) ([]byte, error) {
	decodedSize, err := remoteCaptchaDecodedFrameSize(encoded)
	if err != nil {
		return nil, err
	}
	if decodedSize > remoteCaptchaMaxDecodedFrameSize {
		return nil, errRemoteCaptchaFrameTooLarge
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != decodedSize {
		return nil, errRemoteCaptchaFrameInvalid
	}
	return decoded, nil
}

func remoteCaptchaDecodedFrameSize(encoded string) (int, error) {
	if encoded == "" || len(encoded)%4 != 0 {
		return 0, errRemoteCaptchaFrameInvalid
	}
	decodedSize := base64.StdEncoding.DecodedLen(len(encoded))
	if strings.HasSuffix(encoded, "==") {
		decodedSize -= 2
	} else if strings.HasSuffix(encoded, "=") {
		decodedSize--
	}
	return decodedSize, nil
}

func (s *remoteCaptchaStream) Frames() <-chan []byte {
	if s == nil {
		return nil
	}
	return s.frames
}

func (s *remoteCaptchaStream) DispatchInput(ctx context.Context, payload []byte) error {
	if s == nil {
		return errRemoteCaptchaInputInvalid
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	input, err := decodeRemoteCaptchaInput(payload)
	if err != nil {
		return err
	}
	event, err := validateRemoteCaptchaInput(input)
	if err != nil {
		return err
	}
	if event == nil {
		return nil
	}
	if !s.inputLimiter.Allow() {
		return errRemoteCaptchaInputRate
	}
	select {
	case s.inputSlots <- struct{}{}:
		defer func() { <-s.inputSlots }()
	default:
		return errRemoteCaptchaInputBusy
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()
	if err := callCtx.Err(); err != nil {
		return err
	}
	if err := s.client.Call(callCtx, "Input.dispatchMouseEvent", event, nil); err != nil {
		return fmt.Errorf("dispatch remote CAPTCHA pointer input: %w", err)
	}
	return nil
}

func decodeRemoteCaptchaInput(payload []byte) (remoteCaptchaInputMessage, error) {
	if len(payload) == 0 || len(payload) > remoteCaptchaMaxInputPayloadSize {
		return remoteCaptchaInputMessage{}, errRemoteCaptchaInputInvalid
	}
	if !validRemoteCaptchaJSONObject(payload) {
		return remoteCaptchaInputMessage{}, errRemoteCaptchaInputInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var input remoteCaptchaInputMessage
	if err := decoder.Decode(&input); err != nil {
		return remoteCaptchaInputMessage{}, errRemoteCaptchaInputInvalid
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return remoteCaptchaInputMessage{}, errRemoteCaptchaInputInvalid
	}
	return input, nil
}

func validRemoteCaptchaJSONObject(payload []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return false
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		switch key {
		case "type", "phase", "x", "y", "width", "height", "button", "deltaY":
		default:
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return false
	}
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}

func validateRemoteCaptchaInput(input remoteCaptchaInputMessage) (*remoteCaptchaMouseEvent, error) {
	if !validRemoteCaptchaViewportMetadata(input.Width, input.Height) {
		return nil, errRemoteCaptchaInputInvalid
	}
	switch input.Type {
	case "pointer":
		if input.X == nil || input.Y == nil || input.Button == nil || *input.Button != 0 || input.DeltaY != nil ||
			!finiteRemoteCaptchaNumber(*input.X) || !finiteRemoteCaptchaNumber(*input.Y) {
			return nil, errRemoteCaptchaInputInvalid
		}
		event := &remoteCaptchaMouseEvent{
			X:           clampRemoteCaptchaCoordinate(*input.X, remoteCaptchaViewportWidth),
			Y:           clampRemoteCaptchaCoordinate(*input.Y, remoteCaptchaViewportHeight),
			PointerType: "mouse",
		}
		switch input.Phase {
		case "move":
			event.Type = "mouseMoved"
			event.Button = "none"
		case "down":
			event.Type = "mousePressed"
			event.Button = "left"
			event.Buttons = 1
			event.ClickCount = 1
		case "up":
			event.Type = "mouseReleased"
			event.Button = "left"
			event.ClickCount = 1
		default:
			return nil, errRemoteCaptchaInputInvalid
		}
		return event, nil
	case "wheel":
		if input.Phase != "" || input.X == nil || input.Y == nil || input.Button != nil || input.DeltaY == nil ||
			!finiteRemoteCaptchaNumber(*input.X) || !finiteRemoteCaptchaNumber(*input.Y) ||
			!finiteRemoteCaptchaNumber(*input.DeltaY) || math.Abs(*input.DeltaY) > remoteCaptchaMaxWheelDelta {
			return nil, errRemoteCaptchaInputInvalid
		}
		return &remoteCaptchaMouseEvent{
			Type:        "mouseWheel",
			X:           clampRemoteCaptchaCoordinate(*input.X, remoteCaptchaViewportWidth),
			Y:           clampRemoteCaptchaCoordinate(*input.Y, remoteCaptchaViewportHeight),
			Button:      "none",
			DeltaY:      *input.DeltaY,
			PointerType: "mouse",
		}, nil
	default:
		return nil, errRemoteCaptchaInputInvalid
	}
}

func validRemoteCaptchaViewportMetadata(width, height *float64) bool {
	if width == nil && height == nil {
		return true
	}
	return width != nil && height != nil && finiteRemoteCaptchaNumber(*width) && finiteRemoteCaptchaNumber(*height) &&
		*width > 0 && *height > 0
}

func finiteRemoteCaptchaNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func clampRemoteCaptchaCoordinate(value float64, maximum int) float64 {
	return math.Max(0, math.Min(value, float64(maximum)))
}

func newRemoteCaptchaInputLimiter(now func() time.Time) *remoteCaptchaInputLimiter {
	if now == nil {
		now = time.Now
	}
	instant := now()
	return &remoteCaptchaInputLimiter{now: now, last: instant, tokens: remoteCaptchaInputBurst}
}

func (l *remoteCaptchaInputLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	elapsed := now.Sub(l.last).Seconds()
	if elapsed > 0 {
		l.tokens = math.Min(remoteCaptchaInputBurst, l.tokens+elapsed*remoteCaptchaInputEventsPerSecond)
		l.last = now
	}
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

func (s *remoteCaptchaStream) Done() <-chan struct{} {
	if s == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return s.done
}

func (s *remoteCaptchaStream) Err() error {
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
	default:
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *remoteCaptchaStream) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closeRequested = true
		s.mu.Unlock()
		s.cancel()
	})
	select {
	case <-s.done:
		return s.closeResult()
	default:
	}
	select {
	case <-ctx.Done():
		select {
		case <-s.done:
			return s.closeResult()
		default:
			return ctx.Err()
		}
	case <-s.done:
	}
	return s.closeResult()
}

func (s *remoteCaptchaStream) closeResult() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeRequested && errors.Is(s.terminal, context.Canceled) {
		return s.stopErr
	}
	return s.terminal
}
