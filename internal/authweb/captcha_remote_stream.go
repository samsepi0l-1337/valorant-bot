package authweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"net/url"
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
	remoteCaptchaCaptureInterval      = 250 * time.Millisecond
	remoteCaptchaCaptureInset         = 2.0
)

var (
	errRemoteCaptchaFrameInvalid      = errors.New("remote CAPTCHA frame is invalid")
	errRemoteCaptchaFrameTooLarge     = errors.New("remote CAPTCHA frame exceeds 2 MiB")
	errRemoteCaptchaInputInvalid      = errors.New("remote CAPTCHA input is invalid")
	errRemoteCaptchaInputRate         = errors.New("remote CAPTCHA input rate exceeded")
	errRemoteCaptchaInputBusy         = errors.New("remote CAPTCHA input queue is full")
	errRemoteCaptchaLifetimeRequired  = errors.New("remote CAPTCHA requires all owner lifetimes")
	errRemoteCaptchaChallengeTeardown = errors.New("remote CAPTCHA challenge document ended")
	errRiotCaptchaDocumentChanged     = errors.New("Riot CAPTCHA document changed")
)

type remoteCaptchaScreencastFrame struct {
	Data      json.RawMessage                 `json:"data"`
	SessionID int64                           `json:"sessionId"`
	Metadata  remoteCaptchaScreencastMetadata `json:"metadata"`
}

type remoteCaptchaScreencastMetadata struct {
	OffsetTop       float64 `json:"offsetTop"`
	PageScaleFactor float64 `json:"pageScaleFactor"`
	DeviceWidth     float64 `json:"deviceWidth"`
	DeviceHeight    float64 `json:"deviceHeight"`
	ScrollOffsetX   float64 `json:"scrollOffsetX"`
	ScrollOffsetY   float64 `json:"scrollOffsetY"`
	Timestamp       float64 `json:"timestamp"`
}

type remoteCaptchaFrameBinding struct {
	SourceWidth       int
	SourceHeight      int
	FrameWidth        int
	FrameHeight       int
	Crop              image.Rectangle
	Surface           riotCaptchaSurface
	Metadata          remoteCaptchaScreencastMetadata
	Snapshot          riotCaptchaSurfaceSnapshot
	DirectClip        bool
	CapturePageX      float64
	CapturePageY      float64
	CaptureZoom       float64
	CaptureClipX      float64
	CaptureClipY      float64
	CaptureClipWidth  float64
	CaptureClipHeight float64
}

type remoteCaptchaOutputFrame struct {
	JPEG       []byte
	Generation uint64
	Binding    remoteCaptchaFrameBinding
}

type riotCaptchaSurfaceProvider func(context.Context) (riotCaptchaSurface, error)
type riotCaptchaSurfaceSnapshotProvider func(context.Context) (riotCaptchaSurfaceSnapshot, error)
type riotCaptchaInputGuard func(context.Context, riotCaptchaSurfaceSnapshot, float64, float64) error

type remoteCaptchaInputMessage struct {
	Type       string   `json:"type"`
	Phase      string   `json:"phase,omitempty"`
	X          *float64 `json:"x,omitempty"`
	Y          *float64 `json:"y,omitempty"`
	Width      *float64 `json:"width,omitempty"`
	Height     *float64 `json:"height,omitempty"`
	Generation *uint64  `json:"generation,omitempty"`
	Button     *int     `json:"button,omitempty"`
	DeltaY     *float64 `json:"deltaY,omitempty"`
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
	client          *chromeDevToolsClient
	subscription    *chromeDevToolsEventSubscription
	ctx             context.Context
	cancel          context.CancelFunc
	parentStops     []func() bool
	done            chan struct{}
	frames          chan remoteCaptchaOutputFrame
	inputLimiter    *remoteCaptchaInputLimiter
	inputSlots      chan struct{}
	commandTimeout  time.Duration
	surfaceProvider riotCaptchaSurfaceProvider
	captureProvider riotCaptchaSurfaceSnapshotProvider
	inputGuard      riotCaptchaInputGuard
	captureInterval time.Duration
	inputFence      sync.Mutex
	pointerPressed  bool
	lastPointerX    float64
	lastPointerY    float64

	mu                  sync.Mutex
	terminal            error
	stopErr             error
	closeRequested      bool
	lastFrame           remoteCaptchaFrameBinding
	hasLastFrame        bool
	lastGeneration      uint64
	nextGeneration      uint64
	lastOutput          remoteCaptchaOutputFrame
	hasLastOutput       bool
	candidateSurface    riotCaptchaSurface
	hasCandidateSurface bool
	closeOnce           sync.Once
	finishOnce          sync.Once
}

func (c *chromeBrowserController) StartRemoteCaptchaStream(flowCtx, chromeCtx, viewerCtx, shutdownCtx context.Context) (*remoteCaptchaStream, error) {
	if flowCtx == nil || chromeCtx == nil || viewerCtx == nil || shutdownCtx == nil {
		return nil, errRemoteCaptchaLifetimeRequired
	}
	client, err := c.chromeDevToolsClient()
	if err != nil {
		return nil, err
	}
	waitCtx, waitCancel, waitStops := remoteCaptchaLifetimeContext(flowCtx, chromeCtx, viewerCtx, shutdownCtx)
	_, err = c.waitRiotCaptchaSurface(waitCtx)
	waitCancel()
	stopRemoteCaptchaParentWatches(waitStops)
	if err != nil {
		return nil, fmt.Errorf("wait for remote CAPTCHA challenge: %w", err)
	}
	curtainOwnerCtx, curtainOwnerCancel, curtainStops := remoteCaptchaLifetimeContext(flowCtx, chromeCtx, viewerCtx, shutdownCtx)
	curtainCtx, curtainCancel := context.WithTimeout(curtainOwnerCtx, remoteCaptchaCommandTimeout)
	err = c.ensureRiotCaptchaDocumentCurtain(curtainCtx, client)
	curtainCancel()
	curtainOwnerCancel()
	stopRemoteCaptchaParentWatches(curtainStops)
	if err != nil {
		if isExpectedRemoteCaptchaTeardown(err) {
			return nil, fmt.Errorf("%w: install document curtain: %v", errRemoteCaptchaChallengeTeardown, err)
		}
		return nil, fmt.Errorf("install remote CAPTCHA stream curtain: %w", err)
	}
	return newRemoteCaptchaCaptureStream(client, client.currentSessionID(), flowCtx, chromeCtx, viewerCtx, shutdownCtx, client.riotCaptchaSurfaceSnapshot)
}

func newRemoteCaptchaCaptureStream(client *chromeDevToolsClient, sessionID string, flowCtx, chromeCtx, viewerCtx, shutdownCtx context.Context, provider riotCaptchaSurfaceSnapshotProvider) (*remoteCaptchaStream, error) {
	if client == nil || provider == nil {
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
	stream := &remoteCaptchaStream{
		client: client, ctx: ctx, cancel: cancel, parentStops: parentStops,
		done: make(chan struct{}), frames: make(chan remoteCaptchaOutputFrame, 1),
		inputLimiter: newRemoteCaptchaInputLimiter(time.Now), inputSlots: make(chan struct{}, remoteCaptchaMaxOutstandingInput),
		commandTimeout: remoteCaptchaCommandTimeout, captureProvider: provider, captureInterval: remoteCaptchaCaptureInterval,
		inputGuard: client.guardRiotCaptchaInput,
	}
	gateCtx, gateCancel := context.WithTimeout(ctx, remoteCaptchaCommandTimeout)
	err := stream.setInputIgnored(gateCtx, true)
	gateCancel()
	if err != nil {
		cancel()
		stopRemoteCaptchaParentWatches(parentStops)
		if isExpectedRemoteCaptchaTeardown(err) {
			return nil, fmt.Errorf("%w: arm input curtain: %v", errRemoteCaptchaChallengeTeardown, err)
		}
		return nil, fmt.Errorf("arm remote CAPTCHA input curtain: %w", err)
	}
	go stream.runCapture()
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

func (s *remoteCaptchaStream) finish(terminal, stopErr error) {
	s.finishOnce.Do(func() {
		stopRemoteCaptchaParentWatches(s.parentStops)
		s.mu.Lock()
		s.stopErr = stopErr
		s.terminal = errors.Join(terminal, stopErr)
		s.mu.Unlock()
		s.cancel()
		close(s.frames)
		close(s.done)
	})
}

type remoteCaptchaPageIdentity struct {
	FrameID  string
	LoaderID string
}

type remoteCaptchaVisualViewport struct {
	OffsetX      float64 `json:"offsetX"`
	OffsetY      float64 `json:"offsetY"`
	PageX        float64 `json:"pageX"`
	PageY        float64 `json:"pageY"`
	ClientWidth  float64 `json:"clientWidth"`
	ClientHeight float64 `json:"clientHeight"`
	Scale        float64 `json:"scale"`
	Zoom         float64 `json:"zoom"`
}

func (s *remoteCaptchaStream) runCapture() {
	var terminal error
	for terminal == nil {
		terminal = s.captureSanitizedFrame()
		if terminal != nil && isExpectedRemoteCaptchaTeardown(terminal) {
			terminal = errRemoteCaptchaChallengeTeardown
		}
		if terminal != nil {
			break
		}
		interval := s.captureInterval
		if interval <= 0 {
			interval = remoteCaptchaCaptureInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-s.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			terminal = s.ctx.Err()
		case <-timer.C:
		}
	}
	s.finish(terminal, nil)
}

func (s *remoteCaptchaStream) captureSanitizedFrame() error {
	pre, err := s.captureProvider(s.ctx)
	if err != nil {
		if errors.Is(err, errRiotCaptchaSurfaceUnavailable) {
			return nil
		}
		if isExpectedRemoteCaptchaTeardown(err) {
			return errRemoteCaptchaChallengeTeardown
		}
		return fmt.Errorf("inspect pre-capture Riot CAPTCHA surface: %w", err)
	}
	preIdentity, err := s.riotCaptchaPageIdentity(s.ctx)
	if err != nil {
		return err
	}
	viewport, err := s.remoteCaptchaLayoutMetrics(s.ctx)
	if err != nil {
		return fmt.Errorf("inspect remote CAPTCHA viewport: %w", err)
	}
	surface := insetRiotCaptchaSurface(pre.Surface, remoteCaptchaCaptureInset)
	if !validRemoteCaptchaClipSurface(surface) || surface.X+surface.Width > viewport.ClientWidth || surface.Y+surface.Height > viewport.ClientHeight {
		return nil
	}
	captureScale, ok := remoteCaptchaScreenshotScale(viewport, pre.DevicePixelRatio)
	if !ok {
		return errRemoteCaptchaFrameInvalid
	}
	var captured struct {
		Data string `json:"data"`
	}
	if err := s.client.Call(s.ctx, "Page.captureScreenshot", map[string]any{
		"format": "jpeg", "quality": remoteCaptchaJPEGQuality, "fromSurface": true, "captureBeyondViewport": false,
		"clip": map[string]any{
			"x": (viewport.PageX + surface.X) * viewport.Zoom, "y": (viewport.PageY + surface.Y) * viewport.Zoom,
			"width": surface.Width * viewport.Zoom, "height": surface.Height * viewport.Zoom, "scale": captureScale,
		},
	}, &captured); err != nil {
		return fmt.Errorf("capture isolated remote CAPTCHA clip: %w", err)
	}
	post, err := s.captureProvider(s.ctx)
	if err != nil {
		if errors.Is(err, errRiotCaptchaSurfaceUnavailable) {
			return nil
		}
		if isExpectedRemoteCaptchaTeardown(err) {
			return errRemoteCaptchaChallengeTeardown
		}
		return fmt.Errorf("inspect post-capture Riot CAPTCHA surface: %w", err)
	}
	postIdentity, err := s.riotCaptchaPageIdentity(s.ctx)
	if err != nil {
		return err
	}
	if !sameRiotCaptchaCaptureSnapshot(pre, post) || preIdentity != postIdentity {
		return nil
	}
	postViewport, err := s.remoteCaptchaLayoutMetrics(s.ctx)
	if err != nil {
		return fmt.Errorf("inspect post-capture remote CAPTCHA viewport: %w", err)
	}
	if viewport != postViewport {
		return nil
	}
	frame, err := decodeRemoteCaptchaFrame(captured.Data)
	if err != nil {
		return err
	}
	expectedWidth, expectedHeight, ok := remoteCaptchaExpectedImageDimensions(surface, viewport, pre.DevicePixelRatio)
	if !ok {
		return errRemoteCaptchaFrameInvalid
	}
	frame, err = canonicalRemoteCaptchaJPEG(frame, expectedWidth, expectedHeight)
	if err != nil {
		return err
	}
	binding := remoteCaptchaFrameBinding{
		SourceWidth: expectedWidth, SourceHeight: expectedHeight, FrameWidth: expectedWidth, FrameHeight: expectedHeight,
		Crop: image.Rect(0, 0, expectedWidth, expectedHeight), Surface: surface, Snapshot: pre, DirectClip: true,
		Metadata:     remoteCaptchaScreencastMetadata{PageScaleFactor: 1, DeviceWidth: surface.Width, DeviceHeight: surface.Height, Timestamp: float64(time.Now().UnixNano()) / float64(time.Second)},
		CapturePageX: viewport.PageX, CapturePageY: viewport.PageY, CaptureZoom: viewport.Zoom,
		CaptureClipX: (viewport.PageX + surface.X) * viewport.Zoom, CaptureClipY: (viewport.PageY + surface.Y) * viewport.Zoom,
		CaptureClipWidth: surface.Width * viewport.Zoom, CaptureClipHeight: surface.Height * viewport.Zoom,
	}
	return s.queueFrame(s.newOutputFrame(frame, binding))
}

func isExpectedRemoteCaptchaTeardown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errRemoteCaptchaChallengeTeardown) || errors.Is(err, errRiotCaptchaDocumentChanged) {
		return true
	}
	var protocolErr *chromeDevToolsProtocolError
	if !errors.As(err, &protocolErr) {
		return false
	}
	switch protocolErr.Method {
	case "Runtime.evaluate", "Page.getFrameTree", "Page.getLayoutMetrics", "Page.captureScreenshot", "Input.dispatchMouseEvent", "Input.setIgnoreInputEvents":
	default:
		return false
	}
	message := strings.ToLower(protocolErr.Message)
	return strings.Contains(message, "execution context was destroyed") ||
		strings.Contains(message, "cannot find default execution context") ||
		strings.Contains(message, "inspected target navigated or closed") ||
		strings.Contains(message, "not attached to an active page") ||
		strings.Contains(message, "no frame with given id")
}

func (s *remoteCaptchaStream) remoteCaptchaLayoutMetrics(ctx context.Context) (remoteCaptchaVisualViewport, error) {
	var metrics struct {
		CSSVisualViewport remoteCaptchaVisualViewport `json:"cssVisualViewport"`
	}
	if err := s.client.Call(ctx, "Page.getLayoutMetrics", map[string]any{}, &metrics); err != nil {
		return remoteCaptchaVisualViewport{}, err
	}
	if !validRemoteCaptchaVisualViewport(metrics.CSSVisualViewport) {
		return remoteCaptchaVisualViewport{}, errRemoteCaptchaFrameInvalid
	}
	return metrics.CSSVisualViewport, nil
}

func remoteCaptchaExpectedImageDimensions(surface riotCaptchaSurface, viewport remoteCaptchaVisualViewport, devicePixelRatio float64) (int, int, bool) {
	if !validRemoteCaptchaClipSurface(surface) || !validRemoteCaptchaVisualViewport(viewport) ||
		!finiteRemoteCaptchaNumber(devicePixelRatio) || devicePixelRatio <= 0 || devicePixelRatio > 4 {
		return 0, 0, false
	}
	if _, ok := remoteCaptchaScreenshotScale(viewport, devicePixelRatio); !ok {
		return 0, 0, false
	}
	width := surface.Width
	height := surface.Height
	if !finiteRemoteCaptchaNumber(width) || !finiteRemoteCaptchaNumber(height) {
		return 0, 0, false
	}
	expectedWidth, expectedHeight := int(math.Round(width)), int(math.Round(height))
	if expectedWidth <= 0 || expectedHeight <= 0 || expectedWidth > remoteCaptchaViewportWidth || expectedHeight > remoteCaptchaViewportHeight {
		return 0, 0, false
	}
	return expectedWidth, expectedHeight, true
}

func remoteCaptchaScreenshotScale(viewport remoteCaptchaVisualViewport, devicePixelRatio float64) (float64, bool) {
	if !validRemoteCaptchaVisualViewport(viewport) || !finiteRemoteCaptchaNumber(devicePixelRatio) || devicePixelRatio <= 0 || devicePixelRatio > 4 {
		return 0, false
	}
	scale := 1 / devicePixelRatio
	if !finiteRemoteCaptchaNumber(scale) || scale <= 0 || scale > 4 {
		return 0, false
	}
	return scale, true
}

func canonicalRemoteCaptchaJPEG(frame []byte, expectedWidth, expectedHeight int) ([]byte, error) {
	if expectedWidth <= 0 || expectedHeight <= 0 || expectedWidth > remoteCaptchaViewportWidth || expectedHeight > remoteCaptchaViewportHeight ||
		len(frame) == 0 || len(frame) > remoteCaptchaMaxDecodedFrameSize {
		return nil, errRemoteCaptchaFrameInvalid
	}
	if !bytes.HasPrefix(frame, []byte{0xff, 0xd8}) || bytes.Count(frame, []byte{0xff, 0xd8}) != 1 {
		return nil, errRemoteCaptchaFrameInvalid
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(frame))
	if err != nil || config.Width <= 0 || config.Height <= 0 ||
		config.Width < expectedWidth-1 || config.Width > expectedWidth+1 ||
		config.Height < expectedHeight-1 || config.Height > expectedHeight+1 {
		return nil, errRemoteCaptchaFrameInvalid
	}
	decoded, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return nil, errRemoteCaptchaFrameInvalid
	}
	var canonicalImage image.Image = decoded
	if config.Width != expectedWidth || config.Height != expectedHeight {
		resampled := image.NewRGBA(image.Rect(0, 0, expectedWidth, expectedHeight))
		sourceBounds := decoded.Bounds()
		for y := 0; y < expectedHeight; y++ {
			sourceY := sourceBounds.Min.Y + (2*y+1)*config.Height/(2*expectedHeight)
			if sourceY >= sourceBounds.Max.Y {
				sourceY = sourceBounds.Max.Y - 1
			}
			for x := 0; x < expectedWidth; x++ {
				sourceX := sourceBounds.Min.X + (2*x+1)*config.Width/(2*expectedWidth)
				if sourceX >= sourceBounds.Max.X {
					sourceX = sourceBounds.Max.X - 1
				}
				resampled.Set(x, y, decoded.At(sourceX, sourceY))
			}
		}
		canonicalImage = resampled
	}
	var canonical bytes.Buffer
	if err := jpeg.Encode(&canonical, canonicalImage, &jpeg.Options{Quality: remoteCaptchaJPEGQuality}); err != nil || canonical.Len() > remoteCaptchaMaxDecodedFrameSize {
		return nil, errRemoteCaptchaFrameInvalid
	}
	canonicalConfig, err := jpeg.DecodeConfig(bytes.NewReader(canonical.Bytes()))
	if err != nil || canonicalConfig.Width != expectedWidth || canonicalConfig.Height != expectedHeight {
		return nil, errRemoteCaptchaFrameInvalid
	}
	return canonical.Bytes(), nil
}

func insetRiotCaptchaSurface(surface riotCaptchaSurface, inset float64) riotCaptchaSurface {
	left := math.Ceil(surface.X + inset)
	top := math.Ceil(surface.Y + inset)
	right := math.Floor(surface.X + surface.Width - inset)
	bottom := math.Floor(surface.Y + surface.Height - inset)
	return riotCaptchaSurface{X: left, Y: top, Width: right - left, Height: bottom - top}
}

func validRemoteCaptchaClipSurface(surface riotCaptchaSurface) bool {
	return finiteRemoteCaptchaNumber(surface.X) && finiteRemoteCaptchaNumber(surface.Y) && finiteRemoteCaptchaNumber(surface.Width) && finiteRemoteCaptchaNumber(surface.Height) &&
		surface.X >= 0 && surface.Y >= 0 && surface.Width >= 24 && surface.Height >= 24 &&
		surface.X+surface.Width <= remoteCaptchaViewportWidth && surface.Y+surface.Height <= remoteCaptchaViewportHeight
}

func sameRiotCaptchaSnapshot(a, b riotCaptchaSurfaceSnapshot) bool {
	return a.Integrity && b.Integrity && a.Surface == b.Surface && a.DocumentToken != "" &&
		a.DocumentToken == b.DocumentToken && a.SanitizerGeneration != 0 && a.SanitizerGeneration == b.SanitizerGeneration &&
		finiteRemoteCaptchaNumber(a.DevicePixelRatio) && a.DevicePixelRatio > 0 && a.DevicePixelRatio == b.DevicePixelRatio
}

func sameRiotCaptchaCaptureSnapshot(a, b riotCaptchaSurfaceSnapshot) bool {
	return sameRiotCaptchaSnapshot(a, b) && a.MutationEpoch == b.MutationEpoch
}

func validRemoteCaptchaVisualViewport(viewport remoteCaptchaVisualViewport) bool {
	return finiteRemoteCaptchaNumber(viewport.OffsetX) && finiteRemoteCaptchaNumber(viewport.OffsetY) &&
		finiteRemoteCaptchaNumber(viewport.PageX) && finiteRemoteCaptchaNumber(viewport.PageY) &&
		finiteRemoteCaptchaNumber(viewport.ClientWidth) && finiteRemoteCaptchaNumber(viewport.ClientHeight) && finiteRemoteCaptchaNumber(viewport.Scale) &&
		finiteRemoteCaptchaNumber(viewport.Zoom) && viewport.OffsetX == 0 && viewport.OffsetY == 0 &&
		viewport.PageX >= 0 && viewport.PageY >= 0 && viewport.ClientWidth > 0 && viewport.ClientHeight > 0 && viewport.Scale == 1 && viewport.Zoom > 0
}

func (s *remoteCaptchaStream) riotCaptchaPageIdentity(ctx context.Context) (remoteCaptchaPageIdentity, error) {
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID       string `json:"id"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := s.client.Call(ctx, "Page.getFrameTree", map[string]any{}, &tree); err != nil {
		return remoteCaptchaPageIdentity{}, fmt.Errorf("inspect remote CAPTCHA document identity: %w", err)
	}
	parsed, err := url.Parse(tree.FrameTree.Frame.URL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, RiotCaptchaHost) || tree.FrameTree.Frame.ID == "" || tree.FrameTree.Frame.LoaderID == "" {
		return remoteCaptchaPageIdentity{}, fmt.Errorf("inspect remote CAPTCHA document identity: %w: exact authentication origin changed", errRiotCaptchaDocumentChanged)
	}
	return remoteCaptchaPageIdentity{FrameID: tree.FrameTree.Frame.ID, LoaderID: tree.FrameTree.Frame.LoaderID}, nil
}

func (s *remoteCaptchaStream) newOutputFrame(frame []byte, binding remoteCaptchaFrameBinding) remoteCaptchaOutputFrame {
	s.mu.Lock()
	s.nextGeneration++
	generation := s.nextGeneration
	s.mu.Unlock()
	return remoteCaptchaOutputFrame{JPEG: frame, Generation: generation, Binding: binding}
}

func (s *remoteCaptchaStream) queueFrame(frame remoteCaptchaOutputFrame) error {
	s.mu.Lock()
	s.lastOutput = frame
	s.hasLastOutput = true
	s.mu.Unlock()
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

func cropRemoteCaptchaFrame(frame []byte, metadata remoteCaptchaScreencastMetadata, surface riotCaptchaSurface) ([]byte, remoteCaptchaFrameBinding, error) {
	if !validRemoteCaptchaScreencastMetadata(metadata) || !validRiotCaptchaSurface(surface) {
		return nil, remoteCaptchaFrameBinding{}, errRemoteCaptchaFrameInvalid
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(frame))
	if err != nil || config.Width <= 0 || config.Height <= 0 ||
		config.Width > remoteCaptchaViewportWidth || config.Height > remoteCaptchaViewportHeight {
		return nil, remoteCaptchaFrameBinding{}, errRemoteCaptchaFrameInvalid
	}
	screenZoom := float64(config.Width) / metadata.DeviceWidth
	if !finiteRemoteCaptchaNumber(screenZoom) || screenZoom <= 0 {
		return nil, remoteCaptchaFrameBinding{}, errRemoteCaptchaFrameInvalid
	}
	crop := image.Rect(
		int(math.Floor(surface.X*metadata.PageScaleFactor*screenZoom)),
		int(math.Floor((surface.Y*metadata.PageScaleFactor+metadata.OffsetTop)*screenZoom)),
		int(math.Ceil((surface.X+surface.Width)*metadata.PageScaleFactor*screenZoom)),
		int(math.Ceil(((surface.Y+surface.Height)*metadata.PageScaleFactor+metadata.OffsetTop)*screenZoom)),
	)
	fullBounds := image.Rect(0, 0, config.Width, config.Height)
	if crop.Empty() || !crop.In(fullBounds) {
		return nil, remoteCaptchaFrameBinding{}, errRemoteCaptchaFrameInvalid
	}
	decoded, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return nil, remoteCaptchaFrameBinding{}, errRemoteCaptchaFrameInvalid
	}
	type subImager interface {
		SubImage(image.Rectangle) image.Image
	}
	imageWithCrop, ok := decoded.(subImager)
	if !ok {
		return nil, remoteCaptchaFrameBinding{}, errRemoteCaptchaFrameInvalid
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, imageWithCrop.SubImage(crop), &jpeg.Options{Quality: remoteCaptchaJPEGQuality}); err != nil {
		return nil, remoteCaptchaFrameBinding{}, errRemoteCaptchaFrameInvalid
	}
	binding := remoteCaptchaFrameBinding{
		SourceWidth: config.Width, SourceHeight: config.Height,
		FrameWidth: crop.Dx(), FrameHeight: crop.Dy(), Crop: crop,
		Surface: surface, Metadata: metadata,
	}
	return encoded.Bytes(), binding, nil
}

func validRemoteCaptchaScreencastMetadata(metadata remoteCaptchaScreencastMetadata) bool {
	values := [...]float64{
		metadata.OffsetTop, metadata.PageScaleFactor, metadata.DeviceWidth, metadata.DeviceHeight,
		metadata.ScrollOffsetX, metadata.ScrollOffsetY, metadata.Timestamp,
	}
	for _, value := range values {
		if !finiteRemoteCaptchaNumber(value) {
			return false
		}
	}
	return metadata.OffsetTop >= 0 && metadata.PageScaleFactor > 0 && metadata.DeviceWidth > 0 && metadata.DeviceHeight > 0
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

func (s *remoteCaptchaStream) Frames() <-chan remoteCaptchaOutputFrame {
	if s == nil {
		return nil
	}
	return s.frames
}

func (s *remoteCaptchaStream) ReplayFrame() (remoteCaptchaOutputFrame, bool) {
	if s == nil {
		return remoteCaptchaOutputFrame{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasLastOutput {
		return remoteCaptchaOutputFrame{}, false
	}
	frame := s.lastOutput
	frame.JPEG = append([]byte(nil), frame.JPEG...)
	return frame, true
}

func (s *remoteCaptchaStream) AcknowledgeFrame(frame remoteCaptchaOutputFrame) error {
	if s == nil || frame.Generation == 0 || len(frame.JPEG) == 0 ||
		frame.Binding.FrameWidth <= 0 || frame.Binding.FrameHeight <= 0 ||
		(!validRiotCaptchaSurface(frame.Binding.Surface) && !(frame.Binding.DirectClip && validRiotCaptchaSurface(frame.Binding.Snapshot.Surface) && validRemoteCaptchaClipSurface(frame.Binding.Surface))) ||
		!validRemoteCaptchaScreencastMetadata(frame.Binding.Metadata) {
		return errRemoteCaptchaInputInvalid
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(frame.JPEG))
	if err != nil || config.Width != frame.Binding.FrameWidth || config.Height != frame.Binding.FrameHeight {
		return errRemoteCaptchaInputInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if frame.Generation > s.nextGeneration || frame.Generation < s.lastGeneration {
		return errRemoteCaptchaInputInvalid
	}
	s.lastFrame = frame.Binding
	s.lastGeneration = frame.Generation
	s.hasLastFrame = true
	return nil
}

func (s *remoteCaptchaStream) DispatchInput(ctx context.Context, payload []byte) (returnErr error) {
	if s == nil {
		return errRemoteCaptchaInputInvalid
	}
	defer func() {
		returnErr = s.normalizeInputTerminal(returnErr)
	}()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	input, err := decodeRemoteCaptchaInput(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	binding := s.lastFrame
	hasLastFrame := s.hasLastFrame
	generation := s.lastGeneration
	requireGeneration := s.surfaceProvider != nil || s.captureProvider != nil
	s.mu.Unlock()
	if !hasLastFrame || (requireGeneration && (input.Generation == nil || *input.Generation != generation)) {
		return errRemoteCaptchaInputInvalid
	}
	event, err := bindRemoteCaptchaInput(input, binding)
	if err != nil {
		return err
	}
	if event == nil {
		return nil
	}
	if event.Type == "mouseReleased" {
		s.inputFence.Lock()
		if !s.pointerPressed {
			s.inputFence.Unlock()
			return nil
		}
	} else {
		if !s.inputLimiter.Allow() {
			return errRemoteCaptchaInputRate
		}
		select {
		case s.inputSlots <- struct{}{}:
			defer func() { <-s.inputSlots }()
		default:
			return errRemoteCaptchaInputBusy
		}
		s.inputFence.Lock()
	}
	defer s.inputFence.Unlock()
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
	if event.Type == "mouseMoved" && s.pointerPressed {
		event.Buttons = 1
	}
	if s.captureProvider != nil {
		if err := s.setInputIgnored(callCtx, true); err != nil {
			if isExpectedRemoteCaptchaTeardown(err) {
				return errRemoteCaptchaChallengeTeardown
			}
			return fmt.Errorf("arm remote CAPTCHA input fence: %w", err)
		}
		current, guardErr := s.captureProvider(callCtx)
		if isExpectedRemoteCaptchaTeardown(guardErr) {
			return errRemoteCaptchaChallengeTeardown
		}
		if guardErr != nil || !sameRiotCaptchaSnapshot(binding.Snapshot, current) || current.Surface != binding.Snapshot.Surface {
			return errRemoteCaptchaInputInvalid
		}
		if s.inputGuard == nil {
			return errRemoteCaptchaInputInvalid
		}
		guardErr = s.inputGuard(callCtx, binding.Snapshot, event.X, event.Y)
		if isExpectedRemoteCaptchaTeardown(guardErr) {
			return errRemoteCaptchaChallengeTeardown
		}
		if guardErr != nil {
			return errRemoteCaptchaInputInvalid
		}
		if err := s.setInputIgnored(callCtx, false); err != nil {
			if isExpectedRemoteCaptchaTeardown(err) {
				return errRemoteCaptchaChallengeTeardown
			}
			return fmt.Errorf("open remote CAPTCHA input fence: %w", err)
		}
		defer func() {
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), remoteCaptchaCommandTimeout)
			restoreErr := s.setInputIgnored(restoreCtx, true)
			restoreCancel()
			if restoreErr != nil {
				if isExpectedRemoteCaptchaTeardown(restoreErr) {
					returnErr = errors.Join(returnErr, errRemoteCaptchaChallengeTeardown)
				} else {
					returnErr = errors.Join(returnErr, fmt.Errorf("restore remote CAPTCHA input fence: %w", restoreErr))
				}
			}
		}()
	}
	if err := s.client.Call(callCtx, "Input.dispatchMouseEvent", event, nil); err != nil {
		if isExpectedRemoteCaptchaTeardown(err) {
			return errRemoteCaptchaChallengeTeardown
		}
		return fmt.Errorf("dispatch remote CAPTCHA pointer input: %w", err)
	}
	switch event.Type {
	case "mousePressed":
		s.pointerPressed = true
		s.lastPointerX, s.lastPointerY = event.X, event.Y
	case "mouseMoved":
		s.lastPointerX, s.lastPointerY = event.X, event.Y
	case "mouseReleased":
		s.pointerPressed = false
		s.lastPointerX, s.lastPointerY = event.X, event.Y
	}
	return nil
}

func (s *remoteCaptchaStream) normalizeInputTerminal(err error) error {
	if err == nil {
		return nil
	}
	s.mu.Lock()
	terminal := s.terminal
	s.mu.Unlock()
	if errors.Is(terminal, errRemoteCaptchaChallengeTeardown) {
		return errRemoteCaptchaChallengeTeardown
	}
	return err
}

func (s *remoteCaptchaStream) ReleasePointer(ctx context.Context) (returnErr error) {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.inputFence.Lock()
	defer s.inputFence.Unlock()
	if !s.pointerPressed {
		return nil
	}
	if errors.Is(s.Err(), errRemoteCaptchaChallengeTeardown) {
		s.pointerPressed = false
		return nil
	}
	callCtx := ctx
	if s.captureProvider != nil {
		if err := s.setInputIgnored(callCtx, false); err != nil {
			if isExpectedRemoteCaptchaTeardown(err) {
				s.pointerPressed = false
				return nil
			}
			return fmt.Errorf("open remote CAPTCHA release fence: %w", err)
		}
		defer func() {
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), remoteCaptchaCommandTimeout)
			restoreErr := s.setInputIgnored(restoreCtx, true)
			restoreCancel()
			if restoreErr != nil && !isExpectedRemoteCaptchaTeardown(restoreErr) {
				returnErr = errors.Join(returnErr, fmt.Errorf("restore remote CAPTCHA release fence: %w", restoreErr))
			}
		}()
	}
	event := &remoteCaptchaMouseEvent{
		Type: "mouseReleased", X: s.lastPointerX, Y: s.lastPointerY,
		Button: "left", Buttons: 0, ClickCount: 1, PointerType: "mouse",
	}
	if err := s.client.Call(callCtx, "Input.dispatchMouseEvent", event, nil); err != nil {
		if isExpectedRemoteCaptchaTeardown(err) {
			s.pointerPressed = false
			return nil
		}
		return fmt.Errorf("release remote CAPTCHA pointer input: %w", err)
	}
	s.pointerPressed = false
	return nil
}

func (s *remoteCaptchaStream) setInputIgnored(ctx context.Context, ignored bool) error {
	if s == nil || s.client == nil {
		return errChromeDevToolsClientClosed
	}
	return s.client.Call(ctx, "Input.setIgnoreInputEvents", map[string]any{"ignore": ignored}, nil)
}

func bindRemoteCaptchaInput(input remoteCaptchaInputMessage, binding remoteCaptchaFrameBinding) (*remoteCaptchaMouseEvent, error) {
	if input.Width == nil || input.Height == nil ||
		!finiteRemoteCaptchaNumber(*input.Width) || !finiteRemoteCaptchaNumber(*input.Height) ||
		*input.Width != float64(binding.FrameWidth) || *input.Height != float64(binding.FrameHeight) ||
		input.X == nil || input.Y == nil || !finiteRemoteCaptchaNumber(*input.X) || !finiteRemoteCaptchaNumber(*input.Y) ||
		*input.X < 0 || *input.Y < 0 || *input.X >= *input.Width || *input.Y >= *input.Height ||
		binding.SourceWidth <= 0 || binding.Crop.Empty() || !validRemoteCaptchaScreencastMetadata(binding.Metadata) {
		return nil, errRemoteCaptchaInputInvalid
	}
	event, err := validateRemoteCaptchaInput(input)
	if err != nil || event == nil {
		return event, err
	}
	if binding.DirectClip {
		if !finiteRemoteCaptchaNumber(binding.CaptureZoom) || binding.CaptureZoom <= 0 ||
			!finiteRemoteCaptchaNumber(binding.CaptureClipX) || !finiteRemoteCaptchaNumber(binding.CaptureClipY) ||
			!finiteRemoteCaptchaNumber(binding.CaptureClipWidth) || !finiteRemoteCaptchaNumber(binding.CaptureClipHeight) ||
			binding.CaptureClipWidth <= 0 || binding.CaptureClipHeight <= 0 {
			return nil, errRemoteCaptchaInputInvalid
		}
		event.X = (binding.CaptureClipX+*input.X/float64(binding.FrameWidth)*binding.CaptureClipWidth)/binding.CaptureZoom - binding.CapturePageX
		event.Y = (binding.CaptureClipY+*input.Y/float64(binding.FrameHeight)*binding.CaptureClipHeight)/binding.CaptureZoom - binding.CapturePageY
		if event.Type == "mouseWheel" {
			event.DeltaY *= binding.CaptureClipHeight / float64(binding.FrameHeight) / binding.CaptureZoom
		}
		return event, nil
	}
	screenZoom := float64(binding.SourceWidth) / binding.Metadata.DeviceWidth
	pageScale := binding.Metadata.PageScaleFactor
	if !finiteRemoteCaptchaNumber(screenZoom) || screenZoom <= 0 || !finiteRemoteCaptchaNumber(pageScale) || pageScale <= 0 {
		return nil, errRemoteCaptchaInputInvalid
	}
	event.X = (float64(binding.Crop.Min.X) + *input.X) / screenZoom / pageScale
	event.Y = ((float64(binding.Crop.Min.Y)+*input.Y)/screenZoom - binding.Metadata.OffsetTop) / pageScale
	if !finiteRemoteCaptchaNumber(event.X) || !finiteRemoteCaptchaNumber(event.Y) ||
		event.X < binding.Surface.X || event.X >= binding.Surface.X+binding.Surface.Width ||
		event.Y < binding.Surface.Y || event.Y >= binding.Surface.Y+binding.Surface.Height {
		return nil, errRemoteCaptchaInputInvalid
	}
	if event.Type == "mouseWheel" {
		event.DeltaY /= screenZoom * pageScale
	}
	return event, nil
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
		case "type", "phase", "x", "y", "width", "height", "generation", "button", "deltaY":
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
