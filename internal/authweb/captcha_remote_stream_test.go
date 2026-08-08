package authweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

type remoteCaptchaTestCommand struct {
	ID        int64  `json:"id"`
	Method    string `json:"method"`
	SessionID string `json:"sessionId"`
	Params    struct {
		Format         string  `json:"format"`
		Quality        int     `json:"quality"`
		MaxWidth       int     `json:"maxWidth"`
		MaxHeight      int     `json:"maxHeight"`
		FrameSessionID int64   `json:"sessionId"`
		MouseType      string  `json:"type"`
		X              float64 `json:"x"`
		Y              float64 `json:"y"`
		Button         string  `json:"button"`
		Buttons        int     `json:"buttons"`
		ClickCount     int     `json:"clickCount"`
		DeltaX         float64 `json:"deltaX"`
		DeltaY         float64 `json:"deltaY"`
		PointerType    string  `json:"pointerType"`
	} `json:"params"`
}

func nextRemoteCaptchaTestCommand(t *testing.T, browser *chromeDevToolsPipe) remoteCaptchaTestCommand {
	t.Helper()
	type result struct {
		command remoteCaptchaTestCommand
		err     error
	}
	done := make(chan result, 1)
	go func() {
		var command remoteCaptchaTestCommand
		err := browser.ReadJSON(&command)
		done <- result{command: command, err: err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.command
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Chrome DevTools command")
		return remoteCaptchaTestCommand{}
	}
}

func replyRemoteCaptchaTestCommand(t *testing.T, browser *chromeDevToolsPipe, id int64) {
	t.Helper()
	if err := browser.WriteJSON(map[string]any{"id": id, "result": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
}

func startRemoteCaptchaTestStream(t *testing.T, client *chromeDevToolsClient, browser *chromeDevToolsPipe, lifetimes ...context.Context) *remoteCaptchaStream {
	t.Helper()
	type result struct {
		stream *remoteCaptchaStream
		err    error
	}
	done := make(chan result, 1)
	owners := [4]context.Context{context.Background(), context.Background(), context.Background(), context.Background()}
	copy(owners[:], lifetimes)
	go func() {
		stream, err := newRemoteCaptchaStream(client, client.currentSessionID(), owners[0], owners[1], owners[2], owners[3])
		done <- result{stream: stream, err: err}
	}()
	start := nextRemoteCaptchaTestCommand(t, browser)
	if start.Method != "Page.startScreencast" {
		t.Fatalf("first stream command=%q, want Page.startScreencast", start.Method)
	}
	replyRemoteCaptchaTestCommand(t, browser, start.ID)
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.stream
	case <-time.After(time.Second):
		t.Fatal("remote CAPTCHA stream did not start")
		return nil
	}
}

func startRemoteCaptchaControllerTestStream(t *testing.T, controller *chromeBrowserController, browser *chromeDevToolsPipe, owners [4]context.Context) *remoteCaptchaStream {
	t.Helper()
	type result struct {
		stream *remoteCaptchaStream
		err    error
	}
	done := make(chan result, 1)
	go func() {
		stream, err := controller.StartRemoteCaptchaStream(owners[0], owners[1], owners[2], owners[3])
		done <- result{stream: stream, err: err}
	}()
	start := nextRemoteCaptchaTestCommand(t, browser)
	if start.Method != "Page.startScreencast" {
		t.Fatalf("first controller stream command=%q, want Page.startScreencast", start.Method)
	}
	replyRemoteCaptchaTestCommand(t, browser, start.ID)
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.stream
	case <-time.After(time.Second):
		t.Fatal("controller remote CAPTCHA stream did not start")
		return nil
	}
}

func closeRemoteCaptchaTestStream(t *testing.T, stream *remoteCaptchaStream, browser *chromeDevToolsPipe) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- stream.Close(ctx) }()
	stop := nextRemoteCaptchaTestCommand(t, browser)
	if stop.Method != "Page.stopScreencast" {
		t.Fatalf("close command=%q, want Page.stopScreencast", stop.Method)
	}
	replyRemoteCaptchaTestCommand(t, browser, stop.ID)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("remote CAPTCHA stream did not close")
	}
}

func TestRemoteCaptchaStreamStartsJPEGAndStopsOnClose(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")

	type startResult struct {
		stream *remoteCaptchaStream
		err    error
	}
	started := make(chan startResult, 1)
	go func() {
		stream, err := newRemoteCaptchaStream(client, "riot-session", context.Background(), context.Background(), context.Background(), context.Background())
		started <- startResult{stream: stream, err: err}
	}()

	start := nextRemoteCaptchaTestCommand(t, browser)
	if start.Method != "Page.startScreencast" || start.SessionID != "riot-session" {
		t.Fatalf("start command method=%q session=%q", start.Method, start.SessionID)
	}
	if start.Params.Format != "jpeg" || start.Params.Quality != 80 ||
		start.Params.MaxWidth != 1280 || start.Params.MaxHeight != 900 {
		t.Fatalf("start params=%+v", start.Params)
	}
	replyRemoteCaptchaTestCommand(t, browser, start.ID)
	result := <-started
	if result.err != nil {
		t.Fatal(result.err)
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Second)
	defer cancelClose()
	closed := make(chan error, 1)
	go func() { closed <- result.stream.Close(closeCtx) }()
	stop := nextRemoteCaptchaTestCommand(t, browser)
	if stop.Method != "Page.stopScreencast" || stop.SessionID != "riot-session" {
		t.Fatalf("stop command method=%q session=%q", stop.Method, stop.SessionID)
	}
	replyRemoteCaptchaTestCommand(t, browser, stop.ID)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestRemoteCaptchaStreamControllerOwnsSharedClientAndRequiresFourLifetimes(t *testing.T) {
	t.Run("shared controller client", func(t *testing.T) {
		host, browser := newTestChromeDevToolsPipes()
		t.Cleanup(func() {
			_ = host.Close()
			_ = browser.Close()
		})
		controller := &chromeBrowserController{profileDir: "private-profile", devToolsPipe: host}
		ownedClient, err := controller.chromeDevToolsClient()
		if err != nil {
			t.Fatal(err)
		}
		ownedClient.setSessionID("riot-session")
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
		start := nextRemoteCaptchaTestCommand(t, browser)
		if start.Method != "Page.startScreencast" {
			t.Fatalf("controller start method=%q", start.Method)
		}
		replyRemoteCaptchaTestCommand(t, browser, start.ID)
		startedResult := <-started
		if startedResult.err != nil {
			t.Fatal(startedResult.err)
		}
		if startedResult.stream.client != ownedClient || controller.devToolsClient != ownedClient {
			t.Fatal("controller and remote stream did not reuse one owned DevTools client")
		}
		closeRemoteCaptchaTestStream(t, startedResult.stream, browser)
	})

	for missing, name := range []string{"password flow", "Chrome process", "viewer session", "server shutdown"} {
		t.Run("reject nil "+name, func(t *testing.T) {
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
			contexts := []context.Context{context.Background(), context.Background(), context.Background(), context.Background()}
			contexts[missing] = nil
			stream, err := controller.StartRemoteCaptchaStream(contexts[0], contexts[1], contexts[2], contexts[3])
			if stream != nil || !errors.Is(err, errRemoteCaptchaLifetimeRequired) {
				if stream != nil {
					_ = stream.Close(context.Background())
				}
				t.Fatalf("stream=%p error=%v, want required-lifetime rejection", stream, err)
			}
		})
	}
}

func TestRemoteCaptchaStreamSubscribesBeforeStartAndAcknowledgesFirstFrame(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")

	type startResult struct {
		stream *remoteCaptchaStream
		err    error
	}
	started := make(chan startResult, 1)
	go func() {
		stream, err := newRemoteCaptchaStream(client, "riot-session", context.Background(), context.Background(), context.Background(), context.Background())
		started <- startResult{stream: stream, err: err}
	}()
	start := nextRemoteCaptchaTestCommand(t, browser)
	wantFrame := []byte("first-jpeg-frame")
	if err := browser.WriteJSON(map[string]any{
		"method": "Page.screencastFrame", "sessionId": "riot-session",
		"params": map[string]any{
			"data": base64.StdEncoding.EncodeToString(wantFrame), "sessionId": 41,
		},
	}); err != nil {
		t.Fatal(err)
	}
	replyRemoteCaptchaTestCommand(t, browser, start.ID)
	result := <-started
	if result.err != nil {
		t.Fatal(result.err)
	}

	ack := nextRemoteCaptchaTestCommand(t, browser)
	if ack.Method != "Page.screencastFrameAck" || ack.Params.FrameSessionID != 41 {
		t.Fatalf("ack method=%q params=%+v", ack.Method, ack.Params)
	}
	replyRemoteCaptchaTestCommand(t, browser, ack.ID)
	select {
	case frame := <-result.stream.Frames():
		if !bytes.Equal(frame, wantFrame) {
			t.Fatalf("frame=%q, want %q", frame, wantFrame)
		}
	case <-time.After(time.Second):
		t.Fatal("first screencast frame was not retained")
	}
	closeRemoteCaptchaTestStream(t, result.stream, browser)
}

func TestRemoteCaptchaStreamDropsOldFrameAndRetainsNewest(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())

	var barrierAck remoteCaptchaTestCommand
	for index, frame := range []string{"old-frame", "new-frame", "ack-barrier-frame"} {
		if err := browser.WriteJSON(map[string]any{
			"method": "Page.screencastFrame", "sessionId": "riot-session",
			"params": map[string]any{
				"data": base64.StdEncoding.EncodeToString([]byte(frame)), "sessionId": index + 1,
			},
		}); err != nil {
			t.Fatal(err)
		}
		ack := nextRemoteCaptchaTestCommand(t, browser)
		if ack.Method != "Page.screencastFrameAck" || ack.Params.FrameSessionID != int64(index+1) {
			t.Fatalf("ack %d method=%q params=%+v", index, ack.Method, ack.Params)
		}
		if index < 2 {
			replyRemoteCaptchaTestCommand(t, browser, ack.ID)
			continue
		}
		// Receiving the third ACK proves the second frame has finished replacing
		// the queue, while withholding its response prevents the third frame from
		// entering the queue before the assertion below.
		barrierAck = ack
		break
	}
	if got := cap(stream.Frames()); got != 1 {
		t.Fatalf("frame queue capacity=%d, want 1", got)
	}
	select {
	case frame := <-stream.Frames():
		if string(frame) != "new-frame" {
			t.Fatalf("queued frame=%q, want newest", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("newest frame was not queued")
	}
	replyRemoteCaptchaTestCommand(t, browser, barrierAck.ID)
	select {
	case frame := <-stream.Frames():
		if string(frame) != "ack-barrier-frame" {
			t.Fatalf("barrier frame=%q", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("frame handler did not finish after barrier ACK")
	}
	closeRemoteCaptchaTestStream(t, stream, browser)
}

func TestRemoteCaptchaStreamRejectsOversizedFrameBeforeDecodeAllocation(t *testing.T) {
	encoded := strings.Repeat("A", base64.StdEncoding.EncodedLen(remoteCaptchaMaxDecodedFrameSize+1))
	if allocations := testing.AllocsPerRun(100, func() {
		frame, err := decodeRemoteCaptchaFrame(encoded)
		if frame != nil || !errors.Is(err, errRemoteCaptchaFrameTooLarge) {
			panic("oversized frame was not rejected")
		}
	}); allocations != 0 {
		t.Fatalf("oversized decoded-frame allocations=%v, want 0", allocations)
	}
}

func TestRemoteCaptchaStreamDispatchesValidatedPointerAndWheelInput(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())

	tests := []struct {
		name       string
		payload    string
		mouseType  string
		x, y       float64
		button     string
		buttons    int
		clickCount int
		deltaY     float64
	}{
		{
			name: "move clamps to fixed viewport", payload: `{"type":"pointer","phase":"move","x":-12,"y":999,"width":640,"height":450,"button":0}`,
			mouseType: "mouseMoved", x: 0, y: 900, button: "none",
		},
		{
			name: "primary press", payload: `{"type":"pointer","phase":"down","x":120.5,"y":44.25,"width":640,"height":450,"button":0}`,
			mouseType: "mousePressed", x: 120.5, y: 44.25, button: "left", buttons: 1, clickCount: 1,
		},
		{
			name: "primary release", payload: `{"type":"pointer","phase":"up","x":120.5,"y":44.25,"width":640,"height":450,"button":0}`,
			mouseType: "mouseReleased", x: 120.5, y: 44.25, button: "left", clickCount: 1,
		},
		{
			name: "vertical wheel clamps coordinates", payload: `{"type":"wheel","x":1300,"y":-4,"width":640,"height":450,"deltaY":64}`,
			mouseType: "mouseWheel", x: 1280, y: 0, button: "none", deltaY: 64,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dispatched := make(chan error, 1)
			go func() {
				dispatched <- stream.DispatchInput(context.Background(), []byte(test.payload))
			}()
			command := nextRemoteCaptchaTestCommand(t, browser)
			if command.Method != "Input.dispatchMouseEvent" {
				t.Fatalf("method=%q, want Input.dispatchMouseEvent", command.Method)
			}
			params := command.Params
			if params.MouseType != test.mouseType || params.X != test.x || params.Y != test.y ||
				params.Button != test.button || params.Buttons != test.buttons || params.ClickCount != test.clickCount ||
				params.DeltaX != 0 || params.DeltaY != test.deltaY || params.PointerType != "mouse" {
				t.Fatalf("mouse params=%+v", params)
			}
			replyRemoteCaptchaTestCommand(t, browser, command.ID)
			if err := <-dispatched; err != nil {
				t.Fatal(err)
			}
		})
	}
	closeRemoteCaptchaTestStream(t, stream, browser)
}

func TestRemoteCaptchaStreamRejectsMalformedOrUnsupportedInput(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())

	secret := "raw-secret-must-not-leak"
	invalid := []string{
		`{`,
		strings.Repeat("x", remoteCaptchaMaxInputPayloadSize+1),
		`{"type":"keyboard","key":"Enter"}`,
		`{"type":"pointer","phase":"drag","x":1,"y":2,"button":0}`,
		`{"type":"pointer","phase":"down","x":1,"y":2,"button":1}`,
		`{"type":"pointer","phase":"move","x":1e999,"y":2,"button":0}`,
		`{"type":"wheel","x":1,"y":2,"deltaX":1,"deltaY":2}`,
		`{"type":"wheel","x":1,"y":2,"deltaY":901}`,
		`{"type":"pointer","phase":"move","y":2,"button":0}`,
		`{"type":"pointer","phase":"move","x":1,"y":2,"button":0,"method":"` + secret + `"}`,
		`{"type":"pointer","phase":"move","x":1,"y":2,"button":0} {}`,
	}
	for _, payload := range invalid {
		err := stream.DispatchInput(context.Background(), []byte(payload))
		if !errors.Is(err, errRemoteCaptchaInputInvalid) {
			t.Fatalf("payload %q error=%v, want invalid input", payload, err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("input error leaked raw payload value: %v", err)
		}
	}

	dispatched := make(chan error, 1)
	go func() {
		dispatched <- stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"move","x":3,"y":4,"button":0}`))
	}()
	command := nextRemoteCaptchaTestCommand(t, browser)
	if command.Method != "Input.dispatchMouseEvent" || command.Params.MouseType != "mouseMoved" {
		t.Fatalf("first command after invalid inputs=%+v", command)
	}
	replyRemoteCaptchaTestCommand(t, browser, command.ID)
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	closeRemoteCaptchaTestStream(t, stream, browser)
}

func TestRemoteCaptchaStreamRejectsDuplicateTrailingAndNullJSON(t *testing.T) {
	for _, payload := range []string{
		`{"type":"keyboard","type":"pointer","phase":"move","x":1,"y":2,"button":0}`,
		`{"type":"pointer","phase":"down","x":1,"y":2,"button":1,"button":0}`,
		`{"type":"keyboard","Type":"pointer","phase":"move","x":1,"y":2,"button":0}`,
		`{"type":"pointer","phase":"down","x":1,"y":2,"button":1,"Button":0}`,
		`{"type":"pointer","phase":"move","x":1,"y":2,"button":0,"width":null,"height":null}`,
		`{"type":null,"phase":"move","x":1,"y":2,"button":0}`,
		`null`,
		`{"type":"pointer","phase":"move","x":1,"y":2,"button":0} {}`,
	} {
		input, err := decodeRemoteCaptchaInput([]byte(payload))
		if err == nil {
			_, err = validateRemoteCaptchaInput(input)
		}
		if !errors.Is(err, errRemoteCaptchaInputInvalid) {
			t.Fatalf("payload %q error=%v, want strict JSON rejection", payload, err)
		}
	}
}

func TestRemoteCaptchaStreamRejectsNonFiniteInputValues(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		input := remoteCaptchaInputMessage{Type: "pointer", Phase: "move", X: &value, Y: float64Pointer(1), Button: intPointer(0)}
		if _, err := validateRemoteCaptchaInput(input); !errors.Is(err, errRemoteCaptchaInputInvalid) {
			t.Fatalf("coordinate %v error=%v, want invalid input", value, err)
		}
		input = remoteCaptchaInputMessage{Type: "wheel", X: float64Pointer(1), Y: float64Pointer(1), DeltaY: &value}
		if _, err := validateRemoteCaptchaInput(input); !errors.Is(err, errRemoteCaptchaInputInvalid) {
			t.Fatalf("wheel delta %v error=%v, want invalid input", value, err)
		}
	}
}

func TestRemoteCaptchaStreamEnforces60PerSecondWithBoundedBurst(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := newRemoteCaptchaInputLimiter(func() time.Time { return now })
	for index := 0; index < remoteCaptchaInputBurst; index++ {
		if !limiter.Allow() {
			t.Fatalf("initial event %d rejected within bounded burst", index)
		}
	}
	if limiter.Allow() {
		t.Fatal("event beyond bounded burst was accepted")
	}
	for index := 0; index < remoteCaptchaInputEventsPerSecond; index++ {
		now = now.Add(time.Second/remoteCaptchaInputEventsPerSecond + time.Nanosecond)
		if !limiter.Allow() {
			t.Fatalf("paced event %d rejected below 60 events/second", index)
		}
	}
	if limiter.Allow() {
		t.Fatal("unpaced event above 60 events/second was accepted")
	}
}

func TestRemoteCaptchaStreamAppliesInputRateLimit(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())
	now := time.Unix(200, 0)
	stream.inputLimiter = newRemoteCaptchaInputLimiter(func() time.Time { return now })
	payload := []byte(`{"type":"pointer","phase":"move","x":1,"y":2,"button":0}`)
	for index := 0; index < remoteCaptchaInputBurst; index++ {
		result := make(chan error, 1)
		go func() { result <- stream.DispatchInput(context.Background(), payload) }()
		command := nextRemoteCaptchaTestCommand(t, browser)
		if command.Method != "Input.dispatchMouseEvent" {
			t.Fatalf("burst command %d=%q", index, command.Method)
		}
		replyRemoteCaptchaTestCommand(t, browser, command.ID)
		if err := <-result; err != nil {
			t.Fatalf("burst input %d error=%v", index, err)
		}
	}
	if err := stream.DispatchInput(context.Background(), payload); !errors.Is(err, errRemoteCaptchaInputRate) {
		t.Fatalf("input beyond burst error=%v, want rate limit", err)
	}
	closeRemoteCaptchaTestStream(t, stream, browser)
}

func TestRemoteCaptchaStreamRejectsInputAfterViewerCancellation(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	viewerCtx, cancelViewer := context.WithCancel(context.Background())
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background(), context.Background(), viewerCtx, context.Background())
	cancelViewer()
	stop := nextRemoteCaptchaTestCommand(t, browser)
	if stop.Method != "Page.stopScreencast" {
		t.Fatalf("cancel command=%q, want Page.stopScreencast", stop.Method)
	}
	replyRemoteCaptchaTestCommand(t, browser, stop.ID)
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("stream did not stop after viewer cancellation")
	}
	err := stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"move","x":1,"y":2,"button":0}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("post-cancel input error=%v, want context cancellation", err)
	}
}

func TestRemoteCaptchaStreamStopsWhenAnyOwnedLifetimeEnds(t *testing.T) {
	for canceledIndex, name := range []string{"password flow", "Chrome process", "viewer session", "server shutdown"} {
		t.Run(name, func(t *testing.T) {
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
			lifetimes := [4]context.Context{}
			cancels := make([]context.CancelFunc, 4)
			for index := range lifetimes {
				lifetimes[index], cancels[index] = context.WithCancel(context.Background())
				defer cancels[index]()
			}
			stream := startRemoteCaptchaControllerTestStream(t, controller, browser, lifetimes)
			cancels[canceledIndex]()
			stop := nextRemoteCaptchaTestCommand(t, browser)
			if stop.Method != "Page.stopScreencast" {
				t.Fatalf("lifetime cancellation command=%q, want Page.stopScreencast", stop.Method)
			}
			replyRemoteCaptchaTestCommand(t, browser, stop.ID)
			select {
			case <-stream.Done():
			case <-time.After(time.Second):
				t.Fatal("stream did not stop after owned lifetime ended")
			}
			if !errors.Is(stream.Err(), context.Canceled) {
				t.Fatalf("terminal error=%v, want context cancellation", stream.Err())
			}
		})
	}
}

func TestRemoteCaptchaStreamStopsOnSubscriptionOrClientTermination(t *testing.T) {
	t.Run("subscription", func(t *testing.T) {
		host, browser := newTestChromeDevToolsPipes()
		t.Cleanup(func() {
			_ = host.Close()
			_ = browser.Close()
		})
		client := newChromeDevToolsClient(host)
		client.setSessionID("riot-session")
		stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())
		stream.subscription.Close()
		stop := nextRemoteCaptchaTestCommand(t, browser)
		if stop.Method != "Page.stopScreencast" {
			t.Fatalf("subscription termination command=%q", stop.Method)
		}
		replyRemoteCaptchaTestCommand(t, browser, stop.ID)
		select {
		case <-stream.Done():
		case <-time.After(time.Second):
			t.Fatal("stream did not stop after subscription termination")
		}
		if !errors.Is(stream.Err(), errChromeDevToolsEventSubscriptionClosed) {
			t.Fatalf("terminal error=%v, want subscription closed", stream.Err())
		}
	})

	t.Run("client", func(t *testing.T) {
		host, browser := newTestChromeDevToolsPipes()
		t.Cleanup(func() { _ = host.Close() })
		client := newChromeDevToolsClient(host)
		client.setSessionID("riot-session")
		stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())
		if err := browser.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-stream.Done():
		case <-time.After(time.Second):
			t.Fatal("stream did not stop after client termination")
		}
		if !errors.Is(stream.Err(), errChromeDevToolsClientClosed) {
			t.Fatalf("terminal error=%v, want client closed", stream.Err())
		}
	})
}

func TestRemoteCaptchaStreamConcurrentClosePreservesOneStopFailure(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())

	const callers = 12
	results := make(chan error, callers)
	for range callers {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			results <- stream.Close(ctx)
		}()
	}
	stop := nextRemoteCaptchaTestCommand(t, browser)
	if stop.Method != "Page.stopScreencast" {
		t.Fatalf("close command=%q, want Page.stopScreencast", stop.Method)
	}
	if err := browser.WriteJSON(map[string]any{
		"id": stop.ID, "error": map[string]any{"message": "stop rejected"},
	}); err != nil {
		t.Fatal(err)
	}
	for range callers {
		select {
		case err := <-results:
			if err == nil || !strings.Contains(err.Error(), "stop rejected") {
				t.Fatalf("concurrent Close error=%v, want preserved stop failure", err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent Close did not terminate")
		}
	}
	client.mu.Lock()
	commands := client.nextID
	client.mu.Unlock()
	if commands != 2 {
		t.Fatalf("DevTools command count=%d, want start plus exactly one stop", commands)
	}
	if _, ok := <-stream.Frames(); ok {
		t.Fatal("frame channel remained open after terminal cleanup")
	}
	if err := stream.Err(); err == nil || !strings.Contains(err.Error(), "stop rejected") {
		t.Fatalf("terminal error=%v, want joined stop failure", err)
	}
}

func TestRemoteCaptchaStreamClosePreservesStopTimeout(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())
	stream.commandTimeout = 10 * time.Millisecond

	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result <- stream.Close(ctx)
	}()
	stop := nextRemoteCaptchaTestCommand(t, browser)
	if stop.Method != "Page.stopScreencast" {
		t.Fatalf("close command=%q, want Page.stopScreencast", stop.Method)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close error=%v, want stop timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return after bounded stop timeout")
	}
	if !errors.Is(stream.Err(), context.DeadlineExceeded) {
		t.Fatalf("terminal error=%v, want joined stop timeout", stream.Err())
	}
}

func TestRemoteCaptchaStreamCompletedStopFailureOutranksCanceledCloseContext(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	stream := startRemoteCaptchaTestStream(t, client, browser, ownerCtx)

	cancelOwner()
	stop := nextRemoteCaptchaTestCommand(t, browser)
	if stop.Method != "Page.stopScreencast" {
		t.Fatalf("owner cancellation command=%q, want Page.stopScreencast", stop.Method)
	}
	if err := browser.WriteJSON(map[string]any{
		"id": stop.ID, "error": map[string]any{"message": "completed stop rejected"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("stream did not finish owner cancellation cleanup")
	}

	closeCtx, cancelClose := context.WithCancel(context.Background())
	cancelClose()
	err := stream.Close(closeCtx)
	if err == nil || !strings.Contains(err.Error(), "completed stop rejected") {
		t.Fatalf("Close error=%v, want completed stop failure instead of caller cancellation", err)
	}
}

func TestRemoteCaptchaStreamAcknowledgesOversizedFrameBeforeStopping(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())
	encoded := strings.Repeat("A", base64.StdEncoding.EncodedLen(remoteCaptchaMaxDecodedFrameSize+1))
	if err := browser.WriteJSON(map[string]any{
		"method": "Page.screencastFrame", "sessionId": "riot-session",
		"params": map[string]any{"data": encoded, "sessionId": 72},
	}); err != nil {
		t.Fatal(err)
	}
	ack := nextRemoteCaptchaTestCommand(t, browser)
	if ack.Method != "Page.screencastFrameAck" || ack.Params.FrameSessionID != 72 {
		t.Fatalf("oversized frame ack=%+v", ack)
	}
	replyRemoteCaptchaTestCommand(t, browser, ack.ID)
	stop := nextRemoteCaptchaTestCommand(t, browser)
	if stop.Method != "Page.stopScreencast" {
		t.Fatalf("oversized frame terminal command=%q", stop.Method)
	}
	replyRemoteCaptchaTestCommand(t, browser, stop.ID)
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("stream did not stop after oversized frame")
	}
	if !errors.Is(stream.Err(), errRemoteCaptchaFrameTooLarge) {
		t.Fatalf("terminal error=%v, want oversized frame", stream.Err())
	}
}

func TestRemoteCaptchaStreamAcknowledgesEveryPositiveSessionBeforeDataValidation(t *testing.T) {
	for index, test := range []struct {
		name string
		data any
	}{
		{name: "empty", data: ""},
		{name: "malformed base64", data: "%%%"},
		{name: "null", data: nil},
		{name: "wrong JSON type", data: map[string]any{"unexpected": true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, browser := newTestChromeDevToolsPipes()
			t.Cleanup(func() {
				_ = host.Close()
				_ = browser.Close()
			})
			client := newChromeDevToolsClient(host)
			client.setSessionID("riot-session")
			stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())
			sessionID := int64(index + 1)
			if err := browser.WriteJSON(map[string]any{
				"method": "Page.screencastFrame", "sessionId": "riot-session",
				"params": map[string]any{"data": test.data, "sessionId": sessionID},
			}); err != nil {
				t.Fatal(err)
			}
			ack := nextRemoteCaptchaTestCommand(t, browser)
			if ack.Method != "Page.screencastFrameAck" || ack.Params.FrameSessionID != sessionID {
				t.Fatalf("first command=%+v, want ACK for invalid frame", ack)
			}
			replyRemoteCaptchaTestCommand(t, browser, ack.ID)
			stop := nextRemoteCaptchaTestCommand(t, browser)
			if stop.Method != "Page.stopScreencast" {
				t.Fatalf("terminal command=%q, want stop after ACK", stop.Method)
			}
			replyRemoteCaptchaTestCommand(t, browser, stop.ID)
			select {
			case <-stream.Done():
			case <-time.After(time.Second):
				t.Fatal("invalid frame did not terminate stream")
			}
			if !errors.Is(stream.Err(), errRemoteCaptchaFrameInvalid) {
				t.Fatalf("terminal error=%v, want invalid frame", stream.Err())
			}
		})
	}
}

func TestRemoteCaptchaStreamDoesNotAcknowledgeNonPositiveSession(t *testing.T) {
	for _, sessionID := range []int64{-1, 0} {
		t.Run(fmt.Sprintf("session_%d", sessionID), func(t *testing.T) {
			host, browser := newTestChromeDevToolsPipes()
			t.Cleanup(func() {
				_ = host.Close()
				_ = browser.Close()
			})
			client := newChromeDevToolsClient(host)
			client.setSessionID("riot-session")
			stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())
			if err := browser.WriteJSON(map[string]any{
				"method": "Page.screencastFrame", "sessionId": "riot-session",
				"params": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("frame")), "sessionId": sessionID},
			}); err != nil {
				t.Fatal(err)
			}
			first := nextRemoteCaptchaTestCommand(t, browser)
			if first.Method != "Page.stopScreencast" {
				t.Fatalf("non-positive session command=%q, must not ACK", first.Method)
			}
			replyRemoteCaptchaTestCommand(t, browser, first.ID)
			<-stream.Done()
		})
	}
}

func TestRemoteCaptchaStreamBoundsOutstandingInput(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())

	results := make(chan error, remoteCaptchaMaxOutstandingInput)
	commands := make([]remoteCaptchaTestCommand, 0, remoteCaptchaMaxOutstandingInput)
	for index := 0; index < remoteCaptchaMaxOutstandingInput; index++ {
		go func() {
			results <- stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"move","x":1,"y":2,"button":0}`))
		}()
	}
	for index := 0; index < remoteCaptchaMaxOutstandingInput; index++ {
		command := nextRemoteCaptchaTestCommand(t, browser)
		if command.Method != "Input.dispatchMouseEvent" {
			t.Fatalf("outstanding command %d=%q", index, command.Method)
		}
		commands = append(commands, command)
	}
	if err := stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"move","x":9,"y":2,"button":0}`)); !errors.Is(err, errRemoteCaptchaInputBusy) {
		t.Fatalf("excess outstanding input error=%v, want bounded queue", err)
	}

	for _, command := range commands {
		replyRemoteCaptchaTestCommand(t, browser, command.ID)
	}
	for index := 0; index < remoteCaptchaMaxOutstandingInput; index++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("bounded input returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("bounded input did not finish")
		}
	}
	closeRemoteCaptchaTestStream(t, stream, browser)
}

func TestRemoteCaptchaStreamCancelsPendingInputWithoutPoisoningClient(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- stream.DispatchInput(firstCtx, []byte(`{"type":"pointer","phase":"move","x":1,"y":2,"button":0}`))
	}()
	first := nextRemoteCaptchaTestCommand(t, browser)
	secondResult := make(chan error, 1)
	go func() {
		secondResult <- stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"move","x":3,"y":4,"button":0}`))
	}()
	second := nextRemoteCaptchaTestCommand(t, browser)
	cancelFirst()
	select {
	case err := <-firstResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pending input error=%v, want caller cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending input did not cancel")
	}
	replyRemoteCaptchaTestCommand(t, browser, second.ID)
	if err := <-secondResult; err != nil {
		t.Fatalf("second input failed after first canceled: %v", err)
	}
	// A late response for the canceled request is ignored by the dispatcher.
	replyRemoteCaptchaTestCommand(t, browser, first.ID)
	closeRemoteCaptchaTestStream(t, stream, browser)
}

func TestRemoteCaptchaStreamRejectsViewportMetadataWithoutForwardingCDP(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("riot-session")
	stream := startRemoteCaptchaTestStream(t, client, browser, context.Background())
	if err := stream.DispatchInput(context.Background(), []byte(`{"type":"viewport","width":640,"height":450}`)); !errors.Is(err, errRemoteCaptchaInputInvalid) {
		t.Fatalf("viewport metadata error=%v, want unsupported input", err)
	}

	dispatched := make(chan error, 1)
	go func() {
		dispatched <- stream.DispatchInput(context.Background(), []byte(`{"type":"pointer","phase":"move","x":1,"y":2,"button":0}`))
	}()
	command := nextRemoteCaptchaTestCommand(t, browser)
	if command.Method != "Input.dispatchMouseEvent" {
		t.Fatalf("command after viewport metadata=%q, want fixed mouse dispatch", command.Method)
	}
	replyRemoteCaptchaTestCommand(t, browser, command.ID)
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	closeRemoteCaptchaTestStream(t, stream, browser)
}

func float64Pointer(value float64) *float64 { return &value }

func intPointer(value int) *int { return &value }
