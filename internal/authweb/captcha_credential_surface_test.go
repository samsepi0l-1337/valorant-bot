package authweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRemoteCaptchaControllerWaitsForValidatedChallengeBeforeScreencast(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	controller := &chromeBrowserController{profileDir: "private-profile", devToolsPipe: host}
	client, err := controller.chromeDevToolsClient()
	if err != nil {
		t.Fatal(err)
	}
	client.setSessionID("riot-session")
	waitEntered := make(chan struct{})
	controller.beforeRiotCaptchaReadyWaitForTest = func() { close(waitEntered) }

	type startResult struct {
		stream *remoteCaptchaStream
		err    error
	}
	started := make(chan startResult, 1)
	go func() {
		stream, startErr := controller.StartRemoteCaptchaStream(
			context.Background(), context.Background(), context.Background(), context.Background(),
		)
		started <- startResult{stream: stream, err: startErr}
	}()

	select {
	case <-waitEntered:
	case <-time.After(time.Second):
		t.Fatal("stream start did not enter challenge-ready wait")
	}
	client.mu.Lock()
	preChallengeCommands := client.nextID
	client.mu.Unlock()
	if preChallengeCommands != 0 {
		t.Fatalf("pre-challenge CDP commands=%d, want no screencast or remote input", preChallengeCommands)
	}

	controller.publishRiotCaptchaSurface(riotCaptchaSurface{X: 20, Y: 15, Width: 40, Height: 35}, nil)
	replyRemoteCaptchaCurtainInstall(t, browser)
	replyRemoteCaptchaInputFence(t, browser, true)
	result := <-started
	if result.err != nil {
		t.Fatal(result.err)
	}
	firstCaptureCommand := nextRemoteCaptchaTestCommand(t, browser)
	if firstCaptureCommand.Method != "Runtime.evaluate" {
		t.Fatalf("post-challenge command=%q, want sanitizer evaluation and never screencast", firstCaptureCommand.Method)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := result.stream.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRiotCaptchaSurfaceEvaluationRequiresExactOriginAndVisibleHCaptcha(t *testing.T) {
	for _, test := range []struct {
		name      string
		originOK  bool
		challenge bool
		wantErr   string
	}{
		{name: "validated", originOK: true, challenge: true},
		{name: "wrong origin", originOK: false, challenge: true, wantErr: "origin changed"},
		{name: "unrelated iframe", originOK: true, challenge: false, wantErr: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, browser := newTestChromeDevToolsPipes()
			t.Cleanup(func() {
				_ = host.Close()
				_ = browser.Close()
			})
			client := newChromeDevToolsClient(host)
			client.setSessionID("riot-session")
			go func() {
				var command struct {
					ID     int64          `json:"id"`
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if browser.ReadJSON(&command) != nil {
					return
				}
				expression, _ := command.Params["expression"].(string)
				guarded := strings.Contains(expression, `location.origin!=="https://authenticate.riotgames.com"`)
				isolatesTopDocument := strings.Contains(expression, "visibility:hidden!important") &&
					strings.Contains(expression, "pointer-events:none!important") &&
					strings.Contains(expression, "body *::before,body *::after") &&
					strings.Contains(expression, "crypto.getRandomValues") &&
					strings.Contains(expression, "MutationObserver") &&
					strings.Contains(expression, "documentToken") &&
					strings.Contains(expression, "sanitizerGeneration") &&
					strings.Contains(expression, "integrity") &&
					strings.Contains(expression, "remote-captcha-surface")
				findsHCaptcha := isolatesTopDocument && strings.Contains(expression, `root.querySelectorAll('iframe')`) &&
					strings.Contains(expression, `hcaptchaURL(frame.src)`) &&
					!strings.Contains(expression, `marker.includes('hcaptcha')`) &&
					strings.Contains(expression, "getBoundingClientRect")
				broadAncestorSelector := strings.Contains(expression, `document.querySelectorAll('[data-hcaptcha-widget-id],.h-captcha')`) ||
					strings.Contains(expression, `[class*="hcaptcha"`) || strings.Contains(expression, `[id*="hcaptcha"`)
				width, height := 40.0, 35.0
				if broadAncestorSelector {
					width, height = 1000, 700
				}
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": "riot-session", "result": map[string]any{
					"result": map[string]any{"type": "object", "value": map[string]any{
						"originOK": test.originOK && guarded,
						"ready":    test.challenge && findsHCaptcha,
						"x":        20.0, "y": 15.0, "width": width, "height": height,
					}},
				}})
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			surface, err := client.riotCaptchaSurface(ctx)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("surface error=%v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if surface != (riotCaptchaSurface{X: 20, Y: 15, Width: 40, Height: 35}) {
				t.Fatalf("surface=%+v", surface)
			}
		})
	}
}

func TestRiotCaptchaSanitizerIsDenyByDefaultAndScrubsCredentialElements(t *testing.T) {
	for _, required := range []string{
		"opacity:0!important", "body *::before,body *::after", "content:none!important",
		"element.style.setProperty('visibility','hidden','important')",
		"element.style.setProperty('opacity','0','important')",
		"element.style.setProperty('pointer-events','none','important')",
		"element.value=''", "element.removeAttribute('value')", "element.removeAttribute('placeholder')",
		"curtain.armed=integrity", "hcaptchaURL(selected.src)",
		"Math.round(rect.left)", "Math.round(rect.top)", "Math.round(rect.right)", "Math.round(rect.bottom)",
	} {
		if !strings.Contains(riotCaptchaSurfaceExpression, required) {
			t.Fatalf("sanitizer expression is missing %q", required)
		}
	}
	for _, forbidden := range []string{"[class*=\"hcaptcha\"]", "[id*=\"hcaptcha\"]", ".h-captcha"} {
		if strings.Contains(riotCaptchaSurfaceExpression, forbidden) {
			t.Fatalf("sanitizer trusts broad credential-containing ancestor selector %q", forbidden)
		}
	}
}

func TestRiotCaptchaCurtainAndSanitizerSuppressRootBodyPseudosWithRandomSpecificity(t *testing.T) {
	for name, source := range map[string]string{
		"document curtain": riotCaptchaDocumentCurtainScript,
		"active sanitizer": riotCaptchaSurfaceExpression,
	} {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{
				"crypto.getRandomValues", "rootAttribute", "rootValue", "document.documentElement.setAttribute",
				"html::before,html::after,body::before,body::after", "content:none!important", "display:none!important",
			} {
				if !strings.Contains(source, required) {
					t.Fatalf("%s is missing root/body pseudo fail-closed contract %q", name, required)
				}
			}
		})
	}
	if !strings.Contains(riotCaptchaSurfaceExpression, "body iframe['+state.attribute+'=\"'+state.value+'\"]") {
		t.Fatal("active sanitizer no longer preserves only its exact randomized hCaptcha iframe")
	}
	for _, required := range []string{
		"getComputedStyle(document.documentElement,'::before')",
		"getComputedStyle(document.documentElement,'::after')",
		"getComputedStyle(document.body,'::before')",
		"getComputedStyle(document.body,'::after')",
		"state.integrity=integrity",
	} {
		if !strings.Contains(riotCaptchaSurfaceExpression, required) {
			t.Fatalf("active sanitizer does not validate computed pseudo rendering %q", required)
		}
	}
}

func TestRemoteCaptchaProductionStreamCropsFrameAndBindsInputToLastFrame(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	controller := &chromeBrowserController{profileDir: "private-profile", devToolsPipe: host}
	client, err := controller.chromeDevToolsClient()
	if err != nil {
		t.Fatal(err)
	}
	client.setSessionID("riot-session")
	challenge := riotCaptchaSurface{X: 20, Y: 15, Width: 40, Height: 35}
	controller.publishRiotCaptchaSurface(challenge, nil)

	started := make(chan startResultForCredentialSurface, 1)
	go func() {
		stream, startErr := controller.StartRemoteCaptchaStream(
			context.Background(), context.Background(), context.Background(), context.Background(),
		)
		started <- startResultForCredentialSurface{stream: stream, err: startErr}
	}()
	replyRemoteCaptchaCurtainInstall(t, browser)
	replyRemoteCaptchaInputFence(t, browser, true)
	result := <-started
	if result.err != nil {
		t.Fatal(result.err)
	}
	stream := result.stream

	if err := stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"move","x":1,"y":1,"width":40,"height":35,"button":0}`)); !errors.Is(err, errRemoteCaptchaInputInvalid) {
		t.Fatalf("pre-frame input error=%v, want rejection", err)
	}

	fullJPEG := append(credentialSurfaceJPEG(t, 100, 80, image.Rect(20, 15, 60, 50)), []byte("|browser-user|raw-browser-password|")...)
	snapshot := riotCaptchaSurfaceSnapshot{Surface: challenge, DocumentToken: "document-a", SanitizerGeneration: 3, DevicePixelRatio: 1, Integrity: true}
	clippedJPEG := append(credentialSurfaceJPEG(t, 36, 31, image.Rect(0, 0, 36, 31)), []byte("|browser-user|raw-browser-password|second-jpeg-metadata")...)
	replyRemoteCaptchaCaptureCycle(t, browser, snapshot, "loader-a", clippedJPEG)

	var firstFrame remoteCaptchaOutputFrame
	select {
	case firstFrame = <-stream.Frames():
	case <-time.After(time.Second):
		t.Fatal("cropped challenge frame was not emitted")
	}
	if firstFrame.Generation == 0 {
		t.Fatal("cropped challenge frame has no stream-owned generation")
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(firstFrame.JPEG))
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 36 || config.Height != 31 {
		t.Fatalf("remote frame=%dx%d, want inward challenge-only 36x31", config.Width, config.Height)
	}
	if bytes.Equal(firstFrame.JPEG, fullJPEG) || bytes.Contains(firstFrame.JPEG, []byte("browser-user")) || bytes.Contains(firstFrame.JPEG, []byte("raw-browser-password")) {
		t.Fatal("challenge output retained the full page or raw credential bytes")
	}
	relayStream := newTestRemoteCaptchaStream()
	relayStream.frames <- firstFrame
	close(relayStream.frames)
	wsWriter := &credentialSurfaceWebSocketWriter{}
	neverPing := make(chan time.Time)
	if err := writeRemoteCaptchaWebSocket(context.Background(), wsWriter, relayStream, remoteCaptchaWebSocketTiming{
		now: time.Now, after: func(time.Duration) <-chan time.Time { return neverPing },
	}); err != nil {
		t.Fatal(err)
	}
	wsWriter.mu.Lock()
	wsMessages := append([]credentialSurfaceWebSocketMessage(nil), wsWriter.messages...)
	wsWriter.mu.Unlock()
	if len(wsMessages) != 2 || wsMessages[0].messageType != websocket.TextMessage || wsMessages[1].messageType != websocket.BinaryMessage {
		t.Fatalf("WebSocket messages=%+v, want frame metadata then one challenge JPEG", wsMessages)
	}
	for _, message := range wsMessages {
		if bytes.Equal(message.payload, fullJPEG) || bytes.Contains(message.payload, []byte("browser-user")) || bytes.Contains(message.payload, []byte("raw-browser-password")) {
			t.Fatal("WebSocket emitted a full-page JPEG or raw credential bytes")
		}
	}
	if !bytes.Equal(wsMessages[1].payload, firstFrame.JPEG) {
		t.Fatal("WebSocket binary payload was not the production challenge crop")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(firstFrame.JPEG))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decoded.At(18, 15).RGBA()
	if g <= r*2 || g <= b*2 {
		t.Fatalf("cropped center rgb16=(%d,%d,%d), want challenge-green and no credential-red", r, g, b)
	}
	if err := stream.AcknowledgeFrame(firstFrame); err != nil {
		t.Fatalf("acknowledge first displayed frame: %v", err)
	}

	movedChallenge := riotCaptchaSurface{X: 10, Y: 5, Width: 40, Height: 35}
	movedSnapshot := riotCaptchaSurfaceSnapshot{Surface: movedChallenge, DocumentToken: "document-a", SanitizerGeneration: 4, DevicePixelRatio: 1, Integrity: true}

	for _, payload := range []string{
		fmt.Sprintf(`{"type":"pointer","phase":"move","x":1,"y":1,"width":35,"height":31,"generation":%d,"button":0}`, firstFrame.Generation),
		fmt.Sprintf(`{"type":"pointer","phase":"move","x":-1,"y":1,"width":36,"height":31,"generation":%d,"button":0}`, firstFrame.Generation),
		fmt.Sprintf(`{"type":"pointer","phase":"move","x":36,"y":1,"width":36,"height":31,"generation":%d,"button":0}`, firstFrame.Generation),
	} {
		if err := stream.DispatchInput(context.Background(), []byte(payload)); !errors.Is(err, errRemoteCaptchaInputInvalid) {
			t.Fatalf("invalid bound payload=%s error=%v", payload, err)
		}
	}

	dispatched := make(chan error, 1)
	go func() {
		dispatched <- stream.DispatchInput(context.Background(), []byte(fmt.Sprintf(`{"type":"pointer","phase":"down","x":10,"y":5,"width":36,"height":31,"generation":%d,"button":0}`, firstFrame.Generation)))
	}()
	replyRemoteCaptchaInputFence(t, browser, true)
	guard := nextRemoteCaptchaTestCommand(t, browser)
	replyRiotCaptchaSnapshot(t, browser, guard, snapshot)
	replyRiotCaptchaInputGuard(t, browser)
	replyRemoteCaptchaInputFence(t, browser, false)
	input := nextRemoteCaptchaTestCommand(t, browser)
	if input.Method != "Input.dispatchMouseEvent" || input.Params.X != 32 || input.Params.Y != 22 {
		t.Fatalf("mapped challenge input=%+v, want CSS (32,22)", input)
	}
	replyRemoteCaptchaTestCommand(t, browser, input.ID)
	replyRemoteCaptchaInputFence(t, browser, true)
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	replyRemoteCaptchaCaptureCycle(t, browser, movedSnapshot, "loader-a", clippedJPEG)

	var secondFrame remoteCaptchaOutputFrame
	select {
	case secondFrame = <-stream.Frames():
	case <-time.After(time.Second):
		t.Fatal("moved challenge frame was not emitted")
	}
	if secondFrame.Generation <= firstFrame.Generation || secondFrame.Binding.Surface != insetRiotCaptchaSurface(movedChallenge, remoteCaptchaCaptureInset) {
		t.Fatalf("moved frame=%+v, want newer generation bound to %+v", secondFrame, movedChallenge)
	}
	if err := stream.AcknowledgeFrame(secondFrame); err != nil {
		t.Fatal(err)
	}
	oldGenerationPayload := []byte(fmt.Sprintf(`{"type":"pointer","phase":"move","x":10,"y":5,"width":36,"height":31,"generation":%d,"button":0}`, firstFrame.Generation))
	if err := stream.DispatchInput(context.Background(), oldGenerationPayload); !errors.Is(err, errRemoteCaptchaInputInvalid) {
		t.Fatalf("old generation after moved challenge error=%v, want rejection", err)
	}
	secondDispatch := make(chan error, 1)
	go func() {
		secondDispatch <- stream.DispatchInput(context.Background(), []byte(fmt.Sprintf(`{"type":"pointer","phase":"down","x":10,"y":5,"width":36,"height":31,"generation":%d,"button":0}`, secondFrame.Generation)))
	}()
	replyRemoteCaptchaInputFence(t, browser, true)
	secondGuard := nextRemoteCaptchaTestCommand(t, browser)
	replyRiotCaptchaSnapshot(t, browser, secondGuard, movedSnapshot)
	replyRiotCaptchaInputGuard(t, browser)
	replyRemoteCaptchaInputFence(t, browser, false)
	movedInput := nextRemoteCaptchaTestCommand(t, browser)
	if movedInput.Method != "Input.dispatchMouseEvent" || movedInput.Params.X != 22 || movedInput.Params.Y != 12 {
		t.Fatalf("mapped moved-challenge input=%+v, want CSS (22,12)", movedInput)
	}
	replyRemoteCaptchaTestCommand(t, browser, movedInput.ID)
	replyRemoteCaptchaInputFence(t, browser, true)
	if err := <-secondDispatch; err != nil {
		t.Fatal(err)
	}
	closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Second)
	defer cancelClose()
	if err := stream.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteCaptchaCropAndInputMappingUsesPageScaleFactorAndOffsetTop(t *testing.T) {
	surface := riotCaptchaSurface{X: 5, Y: 3, Width: 40, Height: 32}
	metadata := remoteCaptchaScreencastMetadata{
		OffsetTop: 5, PageScaleFactor: 2, DeviceWidth: 100, DeviceHeight: 80,
		ScrollOffsetX: 111, ScrollOffsetY: 222, Timestamp: 125,
	}
	fullJPEG := credentialSurfaceJPEG(t, 200, 160, image.Rect(20, 22, 180, 150))
	cropped, binding, err := cropRemoteCaptchaFrame(fullJPEG, metadata, surface)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Crop != image.Rect(20, 22, 180, 150) {
		t.Fatalf("page-scaled crop=%v, want (20,22)-(180,150) without scroll offsets", binding.Crop)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(cropped))
	if err != nil || config.Width != 160 || config.Height != 128 {
		t.Fatalf("page-scaled crop config=%+v err=%v", config, err)
	}
	event, err := bindRemoteCaptchaInput(remoteCaptchaInputMessage{
		Type: "wheel", X: float64Pointer(80), Y: float64Pointer(64),
		Width: float64Pointer(160), Height: float64Pointer(128), DeltaY: float64Pointer(120),
	}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if event.X != 25 || event.Y != 19 || event.DeltaY != 30 {
		t.Fatalf("inverse page-scaled input=(%v,%v) delta=%v, want CSS (25,19) delta 30 without scroll offsets", event.X, event.Y, event.DeltaY)
	}
}

func TestRemoteCaptchaClipInputMappingUsesVisualViewportZoomAndPageOffset(t *testing.T) {
	binding := remoteCaptchaFrameBinding{
		SourceWidth: 72, SourceHeight: 62, FrameWidth: 72, FrameHeight: 62,
		Crop: image.Rect(0, 0, 72, 62), Surface: riotCaptchaSurface{X: 22, Y: 17, Width: 36, Height: 31}, DirectClip: true,
		CapturePageX: 7, CapturePageY: 9, CaptureZoom: 2,
		CaptureClipX: 58, CaptureClipY: 52, CaptureClipWidth: 72, CaptureClipHeight: 62,
		Metadata: remoteCaptchaScreencastMetadata{PageScaleFactor: 1, DeviceWidth: 36, DeviceHeight: 31},
	}
	event, err := bindRemoteCaptchaInput(remoteCaptchaInputMessage{
		Type: "wheel", X: float64Pointer(20), Y: float64Pointer(10), Width: float64Pointer(72), Height: float64Pointer(62), DeltaY: float64Pointer(120),
	}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if event.X != 32 || event.Y != 22 || event.DeltaY != 60 {
		t.Fatalf("zoomed clip inverse input=(%v,%v) delta=%v, want viewport CSS (32,22) delta 60", event.X, event.Y, event.DeltaY)
	}
	if !validRemoteCaptchaVisualViewport(remoteCaptchaVisualViewport{PageX: 7, PageY: 9, ClientWidth: 100, ClientHeight: 80, Scale: 1, Zoom: 2}) {
		t.Fatal("valid non-unit page zoom was rejected")
	}
	for _, invalid := range []remoteCaptchaVisualViewport{
		{ClientWidth: 100, ClientHeight: 80, Scale: 2, Zoom: 1},
		{ClientWidth: 100, ClientHeight: 80, Scale: 1, Zoom: math.NaN()},
		{OffsetX: 1, ClientWidth: 100, ClientHeight: 80, Scale: 1, Zoom: 1},
	} {
		if validRemoteCaptchaVisualViewport(invalid) {
			t.Fatalf("unsafe visual viewport accepted: %+v", invalid)
		}
	}
}

func TestRemoteCaptchaStreamDropsPreSurfaceFrameAndEmitsNextStableSanitizedFrame(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	go replyRemoteCaptchaFrameACKs(browser)

	surface := riotCaptchaSurface{X: 10, Y: 5, Width: 40, Height: 35}
	stream := &remoteCaptchaStream{
		client: client, ctx: context.Background(), frames: make(chan remoteCaptchaOutputFrame, 1),
		surfaceProvider: func(context.Context) (riotCaptchaSurface, error) { return surface, nil },
	}
	metadata := remoteCaptchaScreencastMetadata{PageScaleFactor: 1, DeviceWidth: 100, DeviceHeight: 80, Timestamp: 1}
	preSurface := append(credentialSurfaceJPEG(t, 100, 80, image.Rect(20, 15, 60, 50)), []byte("|browser-user|raw-browser-password|")...)
	if err := stream.handleScreencastFrame(remoteCaptchaFrameEvent(t, preSurface, 81, metadata)); err != nil {
		t.Fatal(err)
	}
	select {
	case leaked := <-stream.frames:
		t.Fatalf("frame captured before matching surface generation was emitted: generation=%d", leaked.Generation)
	default:
	}

	stable := append(credentialSurfaceJPEG(t, 100, 80, image.Rect(10, 5, 50, 40)), []byte("|browser-user|raw-browser-password|")...)
	if err := stream.handleScreencastFrame(remoteCaptchaFrameEvent(t, stable, 82, metadata)); err != nil {
		t.Fatal(err)
	}
	select {
	case output := <-stream.frames:
		if output.Binding.Surface != surface || bytes.Contains(output.JPEG, []byte("browser-user")) || bytes.Contains(output.JPEG, []byte("raw-browser-password")) {
			t.Fatalf("stable sanitized output=%+v", output)
		}
	case <-time.After(time.Second):
		t.Fatal("stable post-surface frame was not emitted")
	}
}

func TestRemoteCaptchaStreamTreatsTransientIframeAbsenceAsFrameDrop(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	go replyRemoteCaptchaFrameACKs(browser)
	var surfaceCalls atomic.Int32
	surface := riotCaptchaSurface{X: 10, Y: 5, Width: 40, Height: 35}
	stream := &remoteCaptchaStream{
		client: client, ctx: context.Background(), frames: make(chan remoteCaptchaOutputFrame, 1),
		surfaceProvider: func(context.Context) (riotCaptchaSurface, error) {
			if surfaceCalls.Add(1) == 1 {
				return riotCaptchaSurface{}, errRiotCaptchaSurfaceUnavailable
			}
			return surface, nil
		},
	}
	metadata := remoteCaptchaScreencastMetadata{PageScaleFactor: 1, DeviceWidth: 100, DeviceHeight: 80, Timestamp: 2}
	frame := credentialSurfaceJPEG(t, 100, 80, image.Rect(10, 5, 50, 40))
	for sessionID := int64(91); sessionID <= 93; sessionID++ {
		if err := stream.handleScreencastFrame(remoteCaptchaFrameEvent(t, frame, sessionID, metadata)); err != nil {
			t.Fatalf("frame %d transient surface error=%v", sessionID, err)
		}
	}
	select {
	case output := <-stream.frames:
		if output.Binding.Surface != surface {
			t.Fatalf("recovered surface=%+v", output.Binding.Surface)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not recover after transient iframe absence")
	}
}

func TestRemoteCaptchaCaptureDropsPrePostDocumentGenerationAndLoaderMismatch(t *testing.T) {
	base := riotCaptchaSurfaceSnapshot{
		Surface:       riotCaptchaSurface{X: 20, Y: 15, Width: 40, Height: 35},
		DocumentToken: "document-a", SanitizerGeneration: 3, DevicePixelRatio: 1, Integrity: true,
	}
	for _, test := range []struct {
		name       string
		post       riotCaptchaSurfaceSnapshot
		postLoader string
	}{
		{name: "document", post: riotCaptchaSurfaceSnapshot{Surface: base.Surface, DocumentToken: "document-b", SanitizerGeneration: 3, DevicePixelRatio: 1, Integrity: true}, postLoader: "loader-a"},
		{name: "sanitizer generation", post: riotCaptchaSurfaceSnapshot{Surface: base.Surface, DocumentToken: "document-a", SanitizerGeneration: 4, DevicePixelRatio: 1, Integrity: true}, postLoader: "loader-a"},
		{name: "mutation epoch", post: riotCaptchaSurfaceSnapshot{Surface: base.Surface, DocumentToken: "document-a", SanitizerGeneration: 3, MutationEpoch: 1, DevicePixelRatio: 1, Integrity: true}, postLoader: "loader-a"},
		{name: "main loader", post: base, postLoader: "loader-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, browser := newTestChromeDevToolsPipes()
			t.Cleanup(func() { _ = host.Close(); _ = browser.Close() })
			client := newChromeDevToolsClient(host)
			client.setSessionID("riot-session")
			var calls atomic.Int32
			stream := &remoteCaptchaStream{
				client: client, ctx: context.Background(), frames: make(chan remoteCaptchaOutputFrame, 1),
				captureProvider: func(context.Context) (riotCaptchaSurfaceSnapshot, error) {
					if calls.Add(1) == 1 {
						return base, nil
					}
					return test.post, nil
				},
			}
			done := make(chan error, 1)
			go func() { done <- stream.captureSanitizedFrame() }()
			replyRemoteCaptchaPageIdentity(t, browser, "loader-a")
			layout := nextRemoteCaptchaTestCommand(t, browser)
			if layout.Method != "Page.getLayoutMetrics" {
				t.Fatalf("layout command=%q", layout.Method)
			}
			if err := browser.WriteJSON(map[string]any{"id": layout.ID, "sessionId": "riot-session", "result": map[string]any{
				"cssVisualViewport": map[string]any{
					"offsetX": 0, "offsetY": 0, "pageX": 7, "pageY": 9,
					"clientWidth": 100, "clientHeight": 80, "scale": 1, "zoom": 2,
				},
			}}); err != nil {
				t.Fatal(err)
			}
			capture := nextRemoteCaptchaTestCommand(t, browser)
			if capture.Method != "Page.captureScreenshot" || capture.Params.Clip.X != 58 || capture.Params.Clip.Y != 52 ||
				capture.Params.Clip.Width != 72 || capture.Params.Clip.Height != 62 || capture.Params.Clip.Scale != 1 {
				t.Fatalf("zoomed bounded capture=%+v", capture)
			}
			if err := browser.WriteJSON(map[string]any{"id": capture.ID, "sessionId": "riot-session", "result": map[string]any{
				"data": base64.StdEncoding.EncodeToString(credentialSurfaceJPEG(t, 72, 62, image.Rect(0, 0, 72, 62))),
			}}); err != nil {
				t.Fatal(err)
			}
			replyRemoteCaptchaPageIdentity(t, browser, test.postLoader)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			select {
			case leaked := <-stream.frames:
				t.Fatalf("mismatched capture emitted generation %d", leaked.Generation)
			default:
			}
		})
	}
}

func TestRemoteCaptchaCaptureDropsPostViewportMismatch(t *testing.T) {
	base := riotCaptchaSurfaceSnapshot{
		Surface: riotCaptchaSurface{X: 20, Y: 15, Width: 40, Height: 35}, DocumentToken: "document-a",
		SanitizerGeneration: 3, Integrity: true, DevicePixelRatio: 1,
	}
	pre := remoteCaptchaVisualViewport{PageX: 7, PageY: 9, ClientWidth: 100, ClientHeight: 80, Scale: 1, Zoom: 1}
	for _, test := range []struct {
		name string
		post remoteCaptchaVisualViewport
	}{
		{name: "scroll", post: remoteCaptchaVisualViewport{PageX: 8, PageY: 9, ClientWidth: 100, ClientHeight: 80, Scale: 1, Zoom: 1}},
		{name: "zoom", post: remoteCaptchaVisualViewport{PageX: 7, PageY: 9, ClientWidth: 100, ClientHeight: 80, Scale: 1, Zoom: 2}},
		{name: "size", post: remoteCaptchaVisualViewport{PageX: 7, PageY: 9, ClientWidth: 99, ClientHeight: 80, Scale: 1, Zoom: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, browser := newTestChromeDevToolsPipes()
			t.Cleanup(func() { _ = host.Close(); _ = browser.Close() })
			client := newChromeDevToolsClient(host)
			client.setSessionID("riot-session")
			stream := &remoteCaptchaStream{client: client, ctx: context.Background(), frames: make(chan remoteCaptchaOutputFrame, 1), captureProvider: func(context.Context) (riotCaptchaSurfaceSnapshot, error) { return base, nil }}
			done := make(chan error, 1)
			go func() { done <- stream.captureSanitizedFrame() }()
			replyRemoteCaptchaPageIdentity(t, browser, "loader-a")
			replyRemoteCaptchaLayout(t, browser, pre)
			replyRemoteCaptchaScreenshot(t, browser, credentialSurfaceJPEG(t, 36, 31, image.Rect(0, 0, 36, 31)))
			replyRemoteCaptchaPageIdentity(t, browser, "loader-a")
			replyRemoteCaptchaLayout(t, browser, test.post)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			select {
			case frame := <-stream.frames:
				t.Fatalf("post-layout mismatch emitted generation %d", frame.Generation)
			default:
			}
		})
	}
}

func TestRemoteCaptchaCanonicalJPEGRejectsUntrustedScreenshotShapes(t *testing.T) {
	valid := credentialSurfaceJPEG(t, 36, 31, image.Rect(0, 0, 36, 31))
	withCredentialTrailer := append(append([]byte(nil), valid...), []byte("browser-user|raw-browser-password")...)
	canonical, err := canonicalRemoteCaptchaJPEG(withCredentialTrailer, 36, 31)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte("browser-user")) || bytes.Contains(canonical, []byte("raw-browser-password")) || bytes.Equal(canonical, withCredentialTrailer) {
		t.Fatal("canonical JPEG retained source trailer/credential bytes")
	}
	for _, test := range []struct {
		name   string
		frame  []byte
		width  int
		height int
	}{
		{name: "dimensions outside tolerance", frame: valid, width: 34, height: 31},
		{name: "malformed", frame: []byte("not-a-jpeg"), width: 36, height: 31},
		{name: "multiple JPEG images", frame: append(append([]byte(nil), valid...), valid...), width: 36, height: 31},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := canonicalRemoteCaptchaJPEG(test.frame, test.width, test.height); !errors.Is(err, errRemoteCaptchaFrameInvalid) {
				t.Fatalf("canonical error=%v, want invalid-frame rejection", err)
			}
		})
	}
	for _, test := range []struct {
		name         string
		actualWidth  int
		actualHeight int
	}{
		{name: "one pixel smaller", actualWidth: 35, actualHeight: 30},
		{name: "one pixel larger", actualWidth: 37, actualHeight: 32},
	} {
		t.Run(test.name+" is canonically resampled", func(t *testing.T) {
			source := credentialSurfaceJPEG(t, test.actualWidth, test.actualHeight, image.Rect(0, 0, test.actualWidth, test.actualHeight))
			canonical, err := canonicalRemoteCaptchaJPEG(source, 36, 31)
			if err != nil {
				t.Fatal(err)
			}
			config, err := jpeg.DecodeConfig(bytes.NewReader(canonical))
			if err != nil || config.Width != 36 || config.Height != 31 {
				t.Fatalf("canonical dimensions=%dx%d err=%v, want exact CSS 36x31", config.Width, config.Height, err)
			}
		})
	}
	for _, dimensions := range [][2]int{{34, 31}, {38, 31}, {36, 29}, {36, 33}} {
		frame := credentialSurfaceJPEG(t, dimensions[0], dimensions[1], image.Rect(0, 0, dimensions[0], dimensions[1]))
		if _, err := canonicalRemoteCaptchaJPEG(frame, 36, 31); !errors.Is(err, errRemoteCaptchaFrameInvalid) {
			t.Fatalf("canonical accepted %dx%d screenshot outside one-pixel tolerance: %v", dimensions[0], dimensions[1], err)
		}
	}
}

func TestRemoteCaptchaChromiumClipRoundingGridStaysWithinCanonicalTolerance(t *testing.T) {
	// Page.captureScreenshot first converts the positive DIP clip size to an
	// integer gfx::Size, then applies host DSF and clip.scale. With
	// clip.scale=1/devicePixelRatio and DPR=hostDSF*zoom, the result can differ
	// from the requested CSS size by one pixel, but never more for this grid.
	for _, zoom := range []float64{.8, 1.1, 1.25, 1.5, 2} {
		for _, cssSize := range []int{25, 31, 37, 79, 1279} {
			actual := int(math.Round(math.Trunc(float64(cssSize)*zoom) / zoom))
			if delta := actual - cssSize; delta < -1 || delta > 1 {
				t.Fatalf("zoom=%v CSS=%d Chromium=%d exceeds canonical tolerance", zoom, cssSize, actual)
			}
		}
	}
}

func TestRemoteCaptchaCaptureNormalizesDPRAndZoomToBoundedCSSPixels(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() { _ = host.Close(); _ = browser.Close() })
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	snapshot := riotCaptchaSurfaceSnapshot{
		Surface: riotCaptchaSurface{X: 20, Y: 15, Width: 40, Height: 35}, DocumentToken: "document-a",
		SanitizerGeneration: 3, Integrity: true, DevicePixelRatio: 2,
	}
	viewport := remoteCaptchaVisualViewport{PageX: 7, PageY: 9, ClientWidth: 100, ClientHeight: 80, Scale: 1, Zoom: 2}
	stream := &remoteCaptchaStream{
		client: client, ctx: context.Background(), frames: make(chan remoteCaptchaOutputFrame, 1),
		captureProvider: func(context.Context) (riotCaptchaSurfaceSnapshot, error) { return snapshot, nil },
	}
	done := make(chan error, 1)
	go func() { done <- stream.captureSanitizedFrame() }()
	replyRemoteCaptchaPageIdentity(t, browser, "loader-a")
	replyRemoteCaptchaLayout(t, browser, viewport)
	capture := nextRemoteCaptchaTestCommand(t, browser)
	if capture.Method != "Page.captureScreenshot" || capture.Params.Clip.X != 58 || capture.Params.Clip.Y != 52 ||
		capture.Params.Clip.Width != 72 || capture.Params.Clip.Height != 62 || capture.Params.Clip.Scale != .5 {
		t.Fatalf("DPR/zoom normalized capture=%+v, want zoomed clip with DPR-only scale=.5", capture)
	}
	if err := browser.WriteJSON(map[string]any{"id": capture.ID, "sessionId": "riot-session", "result": map[string]any{
		"data": base64.StdEncoding.EncodeToString(credentialSurfaceJPEG(t, 36, 31, image.Rect(0, 0, 36, 31))),
	}}); err != nil {
		t.Fatal(err)
	}
	replyRemoteCaptchaPageIdentity(t, browser, "loader-a")
	replyRemoteCaptchaLayout(t, browser, viewport)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case output := <-stream.frames:
		if output.Binding.FrameWidth != 36 || output.Binding.FrameHeight != 31 ||
			output.Binding.FrameWidth > remoteCaptchaViewportWidth || output.Binding.FrameHeight > remoteCaptchaViewportHeight {
			t.Fatalf("normalized output dimensions=%dx%d", output.Binding.FrameWidth, output.Binding.FrameHeight)
		}
		if output.Binding.CaptureZoom != 2 || output.Binding.CaptureClipWidth != 72 || output.Binding.CaptureClipHeight != 62 {
			t.Fatalf("input binding lost document zoom: %+v", output.Binding)
		}
	case <-time.After(time.Second):
		t.Fatal("normalized frame was not emitted")
	}
}

func TestRemoteCaptchaExpectedImageDimensionsAreCSSBoundedIndependentOfDPRAndZoom(t *testing.T) {
	surface := riotCaptchaSurface{X: 22, Y: 17, Width: 36, Height: 31}
	viewport := remoteCaptchaVisualViewport{ClientWidth: 100, ClientHeight: 80, Scale: 1, Zoom: 2}
	width, height, ok := remoteCaptchaExpectedImageDimensions(surface, viewport, 2)
	if !ok || width != 36 || height != 31 {
		t.Fatalf("DPR-normalized dimensions=%dx%d ok=%t, want CSS-bounded 36x31", width, height, ok)
	}
}

func TestRemoteCaptchaScreenshotScaleRejectsNonFiniteOrAmplifyingGeometry(t *testing.T) {
	validViewport := remoteCaptchaVisualViewport{ClientWidth: 100, ClientHeight: 80, Scale: 1, Zoom: 1}
	for _, test := range []struct {
		name     string
		viewport remoteCaptchaVisualViewport
		dpr      float64
	}{
		{name: "zero DPR", viewport: validViewport, dpr: 0},
		{name: "NaN DPR", viewport: validViewport, dpr: math.NaN()},
		{name: "oversized DPR", viewport: validViewport, dpr: 4.01},
		{name: "unbounded amplification", viewport: validViewport, dpr: .1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if scale, ok := remoteCaptchaScreenshotScale(test.viewport, test.dpr); ok {
				t.Fatalf("unsafe screenshot scale=%v was accepted", scale)
			}
		})
	}
}

func TestRemoteCaptchaExpectedTeardownClassifierIsNarrow(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("wrapped: %w", errRiotCaptchaDocumentChanged),
		&chromeDevToolsProtocolError{Method: "Runtime.evaluate", Message: "Execution context was destroyed."},
		&chromeDevToolsProtocolError{Method: "Page.captureScreenshot", Message: "Inspected target navigated or closed"},
	} {
		if !isExpectedRemoteCaptchaTeardown(err) {
			t.Fatalf("expected teardown was not classified: %v", err)
		}
	}
	for _, err := range []error{
		errChromeDevToolsClientClosed,
		&chromeDevToolsProtocolError{Method: "Network.getResponseBody", Message: "Inspected target navigated or closed"},
		&chromeDevToolsProtocolError{Method: "Page.captureScreenshot", Message: "invalid clip dimensions"},
		errors.New("renderer pipe broke"),
	} {
		if isExpectedRemoteCaptchaTeardown(err) {
			t.Fatalf("real/unrelated failure was hidden as teardown: %v", err)
		}
	}
}

func TestRemoteCaptchaProductionCaptureTreatsTransientIframeAbsenceAsPollDrop(t *testing.T) {
	stream := &remoteCaptchaStream{
		ctx: context.Background(), frames: make(chan remoteCaptchaOutputFrame, 1),
		captureProvider: func(context.Context) (riotCaptchaSurfaceSnapshot, error) {
			return riotCaptchaSurfaceSnapshot{}, errRiotCaptchaSurfaceUnavailable
		},
	}
	if err := stream.captureSanitizedFrame(); err != nil {
		t.Fatalf("transient iframe absence became terminal: %v", err)
	}
	select {
	case frame := <-stream.frames:
		t.Fatalf("transient absence emitted frame generation %d", frame.Generation)
	default:
	}
}

func TestRiotCredentialSubmitClickRunsInsideExactOriginGuardedEvaluation(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	var submitEvaluations atomic.Int32
	var inputCalls atomic.Int32
	go func() {
		for {
			var command struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if browser.ReadJSON(&command) != nil {
				return
			}
			switch command.Method {
			case "Runtime.evaluate":
				expression, _ := command.Params["expression"].(string)
				value := map[string]any{"originOK": true, "filled": true}
				if strings.Contains(expression, "btn-signin-submit") {
					submitEvaluations.Add(1)
					if !strings.Contains(expression, `location.origin!=="https://authenticate.riotgames.com"`) || !strings.Contains(expression, "button.click()") {
						_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{"result": map[string]any{"value": map[string]any{"originOK": false}}}})
						continue
					}
					value = map[string]any{"originOK": true, "submitted": true}
				}
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{"result": map[string]any{"value": value}}})
			case "Input.dispatchMouseEvent":
				inputCalls.Add(1)
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{}})
			}
		}
	}()
	client := newChromeDevToolsClient(host)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := client.submitRiotCredentials(ctx, "browser-user", "browser-password"); err != nil {
		t.Fatal(err)
	}
	if submitEvaluations.Load() != 1 || inputCalls.Load() != 0 {
		t.Fatalf("submit evaluations=%d separate trusted input calls=%d", submitEvaluations.Load(), inputCalls.Load())
	}
}

func TestRemoteCaptchaDispatchUsesIgnoreGateAroundNavigationWindow(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() { _ = host.Close(); _ = browser.Close() })
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	snapshot := riotCaptchaSurfaceSnapshot{
		Surface: riotCaptchaSurface{X: 20, Y: 15, Width: 40, Height: 35}, DocumentToken: "document-a",
		SanitizerGeneration: 3, Integrity: true, DevicePixelRatio: 1,
	}
	binding := remoteCaptchaFrameBinding{
		SourceWidth: 36, SourceHeight: 31, FrameWidth: 36, FrameHeight: 31, Crop: image.Rect(0, 0, 36, 31),
		Surface: insetRiotCaptchaSurface(snapshot.Surface, 2), Snapshot: snapshot, DirectClip: true,
		CaptureZoom: 1, CaptureClipX: 22, CaptureClipY: 17, CaptureClipWidth: 36, CaptureClipHeight: 31,
		Metadata: remoteCaptchaScreencastMetadata{PageScaleFactor: 1, DeviceWidth: 36, DeviceHeight: 31},
	}
	stream := &remoteCaptchaStream{
		client: client, ctx: context.Background(), frames: make(chan remoteCaptchaOutputFrame, 1),
		inputLimiter: newRemoteCaptchaInputLimiter(time.Now), inputSlots: make(chan struct{}, remoteCaptchaMaxOutstandingInput),
		captureProvider: func(context.Context) (riotCaptchaSurfaceSnapshot, error) { return snapshot, nil },
		inputGuard:      client.guardRiotCaptchaInput, lastFrame: binding, hasLastFrame: true, lastGeneration: 7, nextGeneration: 7,
	}
	done := make(chan error, 1)
	go func() {
		done <- stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"down","x":10,"y":5,"width":36,"height":31,"generation":7,"button":0}`))
	}()
	first := nextRemoteCaptchaTestCommand(t, browser)
	if first.Method != "Input.setIgnoreInputEvents" || !first.Params.Ignore {
		t.Fatalf("first navigation-fence command=%+v, want ignore=true before guard", first)
	}
	replyRemoteCaptchaTestCommand(t, browser, first.ID)
	replyRiotCaptchaInputGuard(t, browser)
	allow := nextRemoteCaptchaTestCommand(t, browser)
	if allow.Method != "Input.setIgnoreInputEvents" || allow.Params.Ignore {
		t.Fatalf("post-guard command=%+v, want bounded ignore=false", allow)
	}
	replyRemoteCaptchaTestCommand(t, browser, allow.ID)
	dispatch := nextRemoteCaptchaTestCommand(t, browser)
	if dispatch.Method != "Input.dispatchMouseEvent" {
		t.Fatalf("bounded input command=%q", dispatch.Method)
	}
	replyRemoteCaptchaTestCommand(t, browser, dispatch.ID)
	restore := nextRemoteCaptchaTestCommand(t, browser)
	if restore.Method != "Input.setIgnoreInputEvents" || !restore.Params.Ignore {
		t.Fatalf("post-dispatch fence=%+v, want ignore=true", restore)
	}
	replyRemoteCaptchaTestCommand(t, browser, restore.ID)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRemoteCaptchaDispatchAcceptsSubpixelIframeJitter(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() { _ = host.Close(); _ = browser.Close() })
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	bound := riotCaptchaSurfaceSnapshot{
		Surface: riotCaptchaSurface{X: 20, Y: 15, Width: 40, Height: 35}, DocumentToken: "document-a",
		SanitizerGeneration: 3, Integrity: true, DevicePixelRatio: 1,
	}
	jittered := riotCaptchaSurfaceSnapshot{
		Surface: riotCaptchaSurface{X: 20.4, Y: 14.7, Width: 39.8, Height: 35.2}, DocumentToken: "document-a",
		SanitizerGeneration: 4, Integrity: true, DevicePixelRatio: 1,
	}
	moved := riotCaptchaSurfaceSnapshot{
		Surface: riotCaptchaSurface{X: 80, Y: 15, Width: 40, Height: 35}, DocumentToken: "document-a",
		SanitizerGeneration: 5, Integrity: true, DevicePixelRatio: 1,
	}
	binding := remoteCaptchaFrameBinding{
		SourceWidth: 36, SourceHeight: 31, FrameWidth: 36, FrameHeight: 31, Crop: image.Rect(0, 0, 36, 31),
		Surface: insetRiotCaptchaSurface(bound.Surface, 2), Snapshot: bound, DirectClip: true,
		CaptureZoom: 1, CaptureClipX: 22, CaptureClipY: 17, CaptureClipWidth: 36, CaptureClipHeight: 31,
		Metadata: remoteCaptchaScreencastMetadata{PageScaleFactor: 1, DeviceWidth: 36, DeviceHeight: 31},
	}
	current := bound
	stream := &remoteCaptchaStream{
		client: client, ctx: context.Background(), frames: make(chan remoteCaptchaOutputFrame, 1),
		inputLimiter: newRemoteCaptchaInputLimiter(time.Now), inputSlots: make(chan struct{}, remoteCaptchaMaxOutstandingInput),
		captureProvider: func(context.Context) (riotCaptchaSurfaceSnapshot, error) { return current, nil },
		inputGuard:      client.guardRiotCaptchaInput, lastFrame: binding, hasLastFrame: true, lastGeneration: 7, nextGeneration: 7,
	}

	current = jittered
	done := make(chan error, 1)
	go func() {
		done <- stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"down","x":10,"y":5,"width":36,"height":31,"generation":7,"button":0}`))
	}()
	dispatched := false
	deadline := time.After(time.Second)
	for {
		command := nextRemoteCaptchaTestCommand(t, browser)
		switch command.Method {
		case "Input.setIgnoreInputEvents":
			replyRemoteCaptchaTestCommand(t, browser, command.ID)
		case "Runtime.evaluate":
			if !strings.Contains(command.Params.Expression, "<1") {
				t.Fatal("input guard still uses subpixel-exact iframe bounds")
			}
			if err := browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": "riot-session", "result": map[string]any{
				"result": map[string]any{"type": "object", "value": map[string]any{"originOK": true, "ok": true}},
			}}); err != nil {
				t.Fatal(err)
			}
		case "Input.dispatchMouseEvent":
			dispatched = true
			replyRemoteCaptchaTestCommand(t, browser, command.ID)
		default:
			t.Fatalf("unexpected jitter dispatch command=%q", command.Method)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("subpixel iframe jitter rejected live pointer: %v", err)
			}
			if !dispatched {
				t.Fatal("subpixel pointer succeeded without dispatching a mouse event")
			}
			goto jitterOK
		case <-deadline:
			t.Fatal("timed out dispatching pointer onto a subpixel-stable iframe")
		case <-time.After(30 * time.Millisecond):
		}
	}
jitterOK:

	current = moved
	movedDone := make(chan error, 1)
	go func() {
		movedDone <- stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"move","x":10,"y":5,"width":36,"height":31,"generation":7,"button":0}`))
	}()
	fence := nextRemoteCaptchaTestCommand(t, browser)
	if fence.Method != "Input.setIgnoreInputEvents" {
		t.Fatalf("relocated iframe fence=%q", fence.Method)
	}
	replyRemoteCaptchaTestCommand(t, browser, fence.ID)
	if err := <-movedDone; !errors.Is(err, errRemoteCaptchaInputInvalid) {
		t.Fatalf("relocated iframe error=%v, want rejection", err)
	}
}

func TestRemoteCaptchaDispatchNavigationWindowFailsClosedWithoutSealingAuth(t *testing.T) {
	if !strings.Contains(riotCaptchaDocumentCurtainScript, "state={armed:false") ||
		!strings.Contains(riotCaptchaDocumentCurtainScript, "if(!state.armed") {
		t.Fatal("new documents do not start behind the pointer/render curtain")
	}
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() { _ = host.Close(); _ = browser.Close() })
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	snapshot := riotCaptchaSurfaceSnapshot{
		Surface: riotCaptchaSurface{X: 20, Y: 15, Width: 40, Height: 35}, DocumentToken: "document-a",
		SanitizerGeneration: 3, DevicePixelRatio: 1, Integrity: true,
	}
	binding := remoteCaptchaFrameBinding{
		SourceWidth: 36, SourceHeight: 31, FrameWidth: 36, FrameHeight: 31, Crop: image.Rect(0, 0, 36, 31),
		Surface: insetRiotCaptchaSurface(snapshot.Surface, 2), Snapshot: snapshot, DirectClip: true,
		CaptureZoom: 1, CaptureClipX: 22, CaptureClipY: 17, CaptureClipWidth: 36, CaptureClipHeight: 31,
		Metadata: remoteCaptchaScreencastMetadata{PageScaleFactor: 1, DeviceWidth: 36, DeviceHeight: 31},
	}
	stream := &remoteCaptchaStream{
		client: client, ctx: context.Background(), frames: make(chan remoteCaptchaOutputFrame, 1),
		inputLimiter: newRemoteCaptchaInputLimiter(time.Now), inputSlots: make(chan struct{}, remoteCaptchaMaxOutstandingInput),
		captureProvider: func(context.Context) (riotCaptchaSurfaceSnapshot, error) { return snapshot, nil },
		inputGuard:      client.guardRiotCaptchaInput, lastFrame: binding, hasLastFrame: true, lastGeneration: 7, nextGeneration: 7,
	}
	done := make(chan error, 1)
	go func() {
		done <- stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"down","x":10,"y":5,"width":36,"height":31,"generation":7,"button":0}`))
	}()
	replyRemoteCaptchaInputFence(t, browser, true)
	replyRiotCaptchaInputGuard(t, browser)
	replyRemoteCaptchaInputFence(t, browser, false)
	dispatch := nextRemoteCaptchaTestCommand(t, browser)
	if dispatch.Method != "Input.dispatchMouseEvent" {
		t.Fatalf("navigation-window command=%q", dispatch.Method)
	}
	if err := browser.WriteJSON(map[string]any{"id": dispatch.ID, "sessionId": "riot-session", "error": map[string]any{"message": "Inspected target navigated or closed"}}); err != nil {
		t.Fatal(err)
	}
	replyRemoteCaptchaInputFence(t, browser, true)
	if err := <-done; !errors.Is(err, errRemoteCaptchaChallengeTeardown) {
		t.Fatalf("navigation-window input error=%v, want benign challenge teardown", err)
	}
}

func remoteCaptchaFrameEvent(t *testing.T, frame []byte, sessionID int64, metadata remoteCaptchaScreencastMetadata) chromeDevToolsMessage {
	t.Helper()
	params, err := json.Marshal(remoteCaptchaScreencastFrame{
		Data: json.RawMessage(strconv.Quote(base64.StdEncoding.EncodeToString(frame))), SessionID: sessionID, Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return chromeDevToolsMessage{Method: "Page.screencastFrame", SessionID: "riot-session", Params: params}
}

func replyRemoteCaptchaFrameACKs(browser chromeDevToolsTransport) {
	for {
		var command struct {
			ID int64 `json:"id"`
		}
		if browser.ReadJSON(&command) != nil {
			return
		}
		_ = browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": "riot-session", "result": map[string]any{}})
	}
}

func replyRemoteCaptchaCaptureCycle(t *testing.T, browser *chromeDevToolsPipe, snapshot riotCaptchaSurfaceSnapshot, loaderID string, clippedJPEG []byte) {
	t.Helper()
	pre := nextRemoteCaptchaTestCommand(t, browser)
	replyRiotCaptchaSnapshot(t, browser, pre, snapshot)
	replyRemoteCaptchaPageIdentity(t, browser, loaderID)
	layout := nextRemoteCaptchaTestCommand(t, browser)
	if layout.Method != "Page.getLayoutMetrics" {
		t.Fatalf("capture layout command=%q", layout.Method)
	}
	if err := browser.WriteJSON(map[string]any{"id": layout.ID, "sessionId": "riot-session", "result": map[string]any{
		"cssVisualViewport": map[string]any{"offsetX": 0, "offsetY": 0, "pageX": 0, "pageY": 0, "clientWidth": 100, "clientHeight": 80, "scale": 1, "zoom": 1},
	}}); err != nil {
		t.Fatal(err)
	}
	capture := nextRemoteCaptchaTestCommand(t, browser)
	want := insetRiotCaptchaSurface(snapshot.Surface, remoteCaptchaCaptureInset)
	if capture.Method != "Page.captureScreenshot" || !capture.Params.FromSurface || capture.Params.CaptureBeyondViewport ||
		capture.Params.Clip.X != want.X || capture.Params.Clip.Y != want.Y || capture.Params.Clip.Width != want.Width ||
		capture.Params.Clip.Height != want.Height || capture.Params.Clip.Scale != 1 {
		t.Fatalf("capture command=%+v, want bounded inward challenge clip %+v", capture, want)
	}
	if err := browser.WriteJSON(map[string]any{"id": capture.ID, "sessionId": "riot-session", "result": map[string]any{
		"data": base64.StdEncoding.EncodeToString(clippedJPEG),
	}}); err != nil {
		t.Fatal(err)
	}
	post := nextRemoteCaptchaTestCommand(t, browser)
	replyRiotCaptchaSnapshot(t, browser, post, snapshot)
	replyRemoteCaptchaPageIdentity(t, browser, loaderID)
	replyRemoteCaptchaLayout(t, browser, remoteCaptchaVisualViewport{
		ClientWidth: 100, ClientHeight: 80, Scale: 1, Zoom: 1,
	})
}

func replyRiotCaptchaSnapshot(t *testing.T, browser *chromeDevToolsPipe, command remoteCaptchaTestCommand, snapshot riotCaptchaSurfaceSnapshot) {
	t.Helper()
	if command.Method != "Runtime.evaluate" {
		t.Fatalf("surface command=%q, want Runtime.evaluate", command.Method)
	}
	surface := snapshot.Surface
	if err := browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": "riot-session", "result": map[string]any{
		"result": map[string]any{"type": "object", "value": map[string]any{
			"originOK": true, "ready": true, "x": surface.X, "y": surface.Y, "width": surface.Width, "height": surface.Height,
			"documentToken": snapshot.DocumentToken, "sanitizerGeneration": snapshot.SanitizerGeneration, "mutationEpoch": snapshot.MutationEpoch,
			"devicePixelRatio": snapshot.DevicePixelRatio, "integrity": snapshot.Integrity,
		}},
	}}); err != nil {
		t.Fatal(err)
	}
}

func replyRemoteCaptchaInputFence(t *testing.T, browser *chromeDevToolsPipe, wantIgnored bool) {
	t.Helper()
	command := nextRemoteCaptchaTestCommand(t, browser)
	if command.Method != "Input.setIgnoreInputEvents" || command.Params.Ignore != wantIgnored {
		t.Fatalf("input fence command=%+v, want ignored=%t", command, wantIgnored)
	}
	replyRemoteCaptchaTestCommand(t, browser, command.ID)
}

func replyRemoteCaptchaCurtainInstall(t *testing.T, browser *chromeDevToolsPipe) {
	t.Helper()
	command := nextRemoteCaptchaTestCommand(t, browser)
	if command.Method != "Page.addScriptToEvaluateOnNewDocument" || command.Params.Source != riotCaptchaDocumentCurtainScript || !command.Params.RunImmediately {
		t.Fatalf("document curtain install command=%+v", command)
	}
	if err := browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": "riot-session", "result": map[string]any{"identifier": "remote-captcha-curtain-test"}}); err != nil {
		t.Fatal(err)
	}
}

func replyRemoteCaptchaPageIdentity(t *testing.T, browser *chromeDevToolsPipe, loaderID string) {
	t.Helper()
	command := nextRemoteCaptchaTestCommand(t, browser)
	if command.Method != "Page.getFrameTree" {
		t.Fatalf("identity command=%q", command.Method)
	}
	if err := browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": "riot-session", "result": map[string]any{
		"frameTree": map[string]any{"frame": map[string]any{
			"id": "main-frame", "loaderId": loaderID, "url": "https://authenticate.riotgames.com/",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
}

func replyRemoteCaptchaLayout(t *testing.T, browser *chromeDevToolsPipe, viewport remoteCaptchaVisualViewport) {
	t.Helper()
	command := nextRemoteCaptchaTestCommand(t, browser)
	if command.Method != "Page.getLayoutMetrics" {
		t.Fatalf("layout command=%q", command.Method)
	}
	if err := browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": "riot-session", "result": map[string]any{
		"cssVisualViewport": viewport,
	}}); err != nil {
		t.Fatal(err)
	}
}

func replyRemoteCaptchaScreenshot(t *testing.T, browser *chromeDevToolsPipe, jpegBytes []byte) {
	t.Helper()
	command := nextRemoteCaptchaTestCommand(t, browser)
	if command.Method != "Page.captureScreenshot" {
		t.Fatalf("screenshot command=%q", command.Method)
	}
	if err := browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": "riot-session", "result": map[string]any{
		"data": base64.StdEncoding.EncodeToString(jpegBytes),
	}}); err != nil {
		t.Fatal(err)
	}
}

func replyRiotCaptchaInputGuard(t *testing.T, browser *chromeDevToolsPipe) {
	t.Helper()
	command := nextRemoteCaptchaTestCommand(t, browser)
	if command.Method != "Runtime.evaluate" {
		t.Fatalf("input guard command=%q", command.Method)
	}
	for _, guard := range []string{
		`location.origin!=="https://authenticate.riotgames.com"`, "documentToken", "state.generation", "state.integrity===true", "elementFromPoint",
		"getComputedStyle(document.documentElement,'::before')", "getComputedStyle(document.body,'::after')",
		"<1",
	} {
		if !strings.Contains(command.Params.Expression, guard) {
			t.Fatalf("input guard expression missing %q", guard)
		}
	}
	if strings.Contains(command.Params.Expression, "<.01") {
		t.Fatal("input guard still uses subpixel-exact iframe bounds")
	}
	if err := browser.WriteJSON(map[string]any{"id": command.ID, "sessionId": "riot-session", "result": map[string]any{
		"result": map[string]any{"type": "object", "value": map[string]any{"originOK": true, "ok": true}},
	}}); err != nil {
		t.Fatal(err)
	}
}

type startResultForCredentialSurface struct {
	stream *remoteCaptchaStream
	err    error
}

type credentialSurfaceWebSocketMessage struct {
	messageType int
	payload     []byte
}

type credentialSurfaceWebSocketWriter struct {
	mu              sync.Mutex
	messages        []credentialSurfaceWebSocketMessage
	controlMessages []credentialSurfaceWebSocketMessage
}

func (*credentialSurfaceWebSocketWriter) SetWriteDeadline(time.Time) error { return nil }

func (w *credentialSurfaceWebSocketWriter) WriteMessage(messageType int, payload []byte) error {
	w.mu.Lock()
	w.messages = append(w.messages, credentialSurfaceWebSocketMessage{messageType: messageType, payload: append([]byte(nil), payload...)})
	w.mu.Unlock()
	return nil
}

func (w *credentialSurfaceWebSocketWriter) WriteControl(messageType int, payload []byte, _ time.Time) error {
	w.mu.Lock()
	w.controlMessages = append(w.controlMessages, credentialSurfaceWebSocketMessage{messageType: messageType, payload: append([]byte(nil), payload...)})
	w.mu.Unlock()
	return nil
}

func credentialSurfaceJPEG(t *testing.T, width, height int, challenge image.Rectangle) []byte {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixel := color.RGBA{R: 220, A: 255}
			if image.Pt(x, y).In(challenge) {
				pixel = color.RGBA{G: 220, A: 255}
			}
			frame.Set(x, y, pixel)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestRiotCredentialEvaluationRejectsOriginMismatchBeforeInput(t *testing.T) {
	for _, phase := range []string{"fill", "submit"} {
		t.Run(phase, func(t *testing.T) {
			var evaluateCalls atomic.Int32
			var inputCalls atomic.Int32
			host, browser := newTestChromeDevToolsPipes()
			t.Cleanup(func() {
				_ = host.Close()
				_ = browser.Close()
			})
			go func() {
				for {
					var command struct {
						ID     int64          `json:"id"`
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					if err := browser.ReadJSON(&command); err != nil {
						return
					}
					switch command.Method {
					case "Runtime.evaluate":
						call := evaluateCalls.Add(1)
						expression, _ := command.Params["expression"].(string)
						guarded := strings.Contains(expression, `location.origin!=="https://authenticate.riotgames.com"`)
						originOK := guarded
						if phase == "fill" || call > 1 {
							originOK = false
						}
						value := map[string]any{"originOK": originOK, "filled": true}
						if strings.Contains(expression, "btn-signin-submit") {
							value = map[string]any{"originOK": originOK, "submitted": true}
						}
						_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{
							"result": map[string]any{"type": "object", "value": value},
						}})
					case "Input.dispatchMouseEvent":
						inputCalls.Add(1)
						_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{}})
					default:
						_ = browser.WriteJSON(map[string]any{"id": command.ID, "error": map[string]any{"message": "unexpected method"}})
					}
				}
			}()

			client := newChromeDevToolsClient(host)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := client.submitRiotCredentials(ctx, "browser-user", "browser-password")
			if err == nil || !strings.Contains(err.Error(), "origin changed") {
				t.Fatalf("submit error=%v, want terminal exact-origin rejection", err)
			}
			if got := inputCalls.Load(); got != 0 {
				t.Fatalf("origin mismatch dispatched %d trusted input events", got)
			}
			wantCalls := int32(1)
			if phase == "submit" {
				wantCalls = 2
			}
			if got := evaluateCalls.Load(); got != wantCalls {
				t.Fatalf("Runtime.evaluate calls=%d, want terminal mismatch after %d", got, wantCalls)
			}
		})
	}
}

func TestRiotCredentialEvaluationRejectsDisallowedOriginAfterNavigationRetry(t *testing.T) {
	var calls atomic.Int32
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	go func() {
		for {
			var command struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if browser.ReadJSON(&command) != nil {
				return
			}
			call := calls.Add(1)
			if call == 1 {
				_ = browser.WriteJSON(map[string]any{"id": command.ID, "error": map[string]any{
					"message": "Execution context was destroyed, most likely because of a navigation.",
				}})
				continue
			}
			expression, _ := command.Params["expression"].(string)
			guarded := strings.Contains(expression, `location.origin!=="https://authenticate.riotgames.com"`)
			_ = browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{
				"result": map[string]any{"type": "object", "value": map[string]any{
					"originOK": !guarded, "filled": true,
				}},
			}})
		}
	}()

	client := newChromeDevToolsClient(host)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := client.submitRiotCredentials(ctx, "browser-user", "browser-password")
	if err == nil || !strings.Contains(err.Error(), "origin changed") {
		t.Fatalf("submit error=%v, want disallowed post-navigation origin rejection", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Runtime.evaluate calls=%d, want one navigation retry then origin rejection", got)
	}
}
