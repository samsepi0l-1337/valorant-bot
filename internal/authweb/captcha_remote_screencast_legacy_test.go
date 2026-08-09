package authweb

// This file retains the old screencast transport only as a test fixture for
// generic queue, input, and cancellation tests. Production contains no
// screencast start/stop path; the credential surface uses bounded clip capture.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"strings"
	"time"
)

func newRemoteCaptchaStream(client *chromeDevToolsClient, sessionID string, flowCtx, chromeCtx, viewerCtx, shutdownCtx context.Context) (*remoteCaptchaStream, error) {
	return newRemoteCaptchaStreamWithSurfaceProvider(client, sessionID, flowCtx, chromeCtx, viewerCtx, shutdownCtx, nil)
}

func newRemoteCaptchaStreamWithSurfaceProvider(client *chromeDevToolsClient, sessionID string, flowCtx, chromeCtx, viewerCtx, shutdownCtx context.Context, surfaceProvider riotCaptchaSurfaceProvider) (*remoteCaptchaStream, error) {
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
		return nil, err
	}
	if err := client.Call(ctx, "Page.startScreencast", map[string]any{
		"format": "jpeg", "quality": remoteCaptchaJPEGQuality, "maxWidth": remoteCaptchaViewportWidth,
		"maxHeight": remoteCaptchaViewportHeight, "everyNthFrame": 1,
	}, nil); err != nil {
		subscription.Close()
		cancel()
		stopRemoteCaptchaParentWatches(parentStops)
		return nil, err
	}
	stream := &remoteCaptchaStream{
		client: client, subscription: subscription, ctx: ctx, cancel: cancel, parentStops: parentStops,
		done: make(chan struct{}), frames: make(chan remoteCaptchaOutputFrame, 1),
		inputLimiter: newRemoteCaptchaInputLimiter(time.Now), inputSlots: make(chan struct{}, remoteCaptchaMaxOutstandingInput),
		commandTimeout: remoteCaptchaCommandTimeout, surfaceProvider: surfaceProvider,
	}
	if surfaceProvider == nil {
		stream.lastFrame = remoteCaptchaFrameBinding{
			SourceWidth: remoteCaptchaViewportWidth, SourceHeight: remoteCaptchaViewportHeight,
			FrameWidth: remoteCaptchaViewportWidth, FrameHeight: remoteCaptchaViewportHeight,
			Crop:     image.Rect(0, 0, remoteCaptchaViewportWidth, remoteCaptchaViewportHeight),
			Surface:  riotCaptchaSurface{Width: remoteCaptchaViewportWidth, Height: remoteCaptchaViewportHeight},
			Metadata: remoteCaptchaScreencastMetadata{PageScaleFactor: 1, DeviceWidth: remoteCaptchaViewportWidth, DeviceHeight: remoteCaptchaViewportHeight},
		}
		stream.hasLastFrame = true
	}
	go stream.runLegacyScreencastForTest()
	return stream, nil
}

func (s *remoteCaptchaStream) runLegacyScreencastForTest() {
	var terminal error
	for terminal == nil {
		var event chromeDevToolsMessage
		event, terminal = s.subscription.Next(s.ctx)
		if terminal == nil {
			terminal = s.handleScreencastFrame(event)
		}
	}
	commandTimeout := s.commandTimeout
	if commandTimeout <= 0 {
		commandTimeout = remoteCaptchaCommandTimeout
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), commandTimeout)
	stopErr := s.client.Call(stopCtx, "Page.stopScreencast", map[string]any{}, nil)
	stopCancel()
	s.subscription.Close()
	s.finish(terminal, stopErr)
}

func (s *remoteCaptchaStream) handleScreencastFrame(event chromeDevToolsMessage) error {
	var params remoteCaptchaScreencastFrame
	if err := json.Unmarshal(event.Params, &params); err != nil || params.SessionID <= 0 {
		return errRemoteCaptchaFrameInvalid
	}
	if err := s.client.Call(s.ctx, "Page.screencastFrameAck", map[string]any{"sessionId": params.SessionID}, nil); err != nil {
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
	if s.surfaceProvider != nil {
		surface, surfaceErr := s.surfaceProvider(s.ctx)
		if surfaceErr != nil {
			if errors.Is(surfaceErr, errRiotCaptchaSurfaceUnavailable) {
				s.mu.Lock()
				s.hasCandidateSurface = false
				s.mu.Unlock()
				return nil
			}
			return surfaceErr
		}
		s.mu.Lock()
		stable := s.hasCandidateSurface && s.candidateSurface == surface
		s.candidateSurface = surface
		s.hasCandidateSurface = true
		s.mu.Unlock()
		if !stable {
			return nil
		}
		frame, binding, err := cropRemoteCaptchaFrame(frame, params.Metadata, surface)
		if err != nil {
			return err
		}
		return s.queueFrame(s.newOutputFrame(frame, binding))
	}
	s.mu.Lock()
	binding := s.lastFrame
	s.mu.Unlock()
	return s.queueFrame(s.newOutputFrame(frame, binding))
}
