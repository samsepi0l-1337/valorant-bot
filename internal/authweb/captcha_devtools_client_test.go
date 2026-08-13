package authweb

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestChromeDevToolsPipes() (*chromeDevToolsPipe, *chromeDevToolsPipe) {
	browserCommands, hostCommands := io.Pipe()
	hostResponses, browserResponses := io.Pipe()
	return newChromeDevToolsPipe(hostResponses, hostCommands), newChromeDevToolsPipe(browserCommands, browserResponses)
}

func TestChromeDevToolsCallReattachesAfterLostPageSession(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	client.setSessionID("dead-session")

	type callResult struct {
		value string
		err   error
	}
	done := make(chan callResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		var result struct {
			Value string `json:"value"`
		}
		err := client.Call(ctx, "Runtime.evaluate", map[string]any{"expression": "1"}, &result)
		done <- callResult{value: result.Value, err: err}
	}()

	evaluate := nextRemoteCaptchaTestCommand(t, browser)
	if evaluate.Method != "Runtime.evaluate" || evaluate.SessionID != "dead-session" {
		t.Fatalf("first evaluate=%+v, want dead-session Runtime.evaluate", evaluate)
	}
	if err := browser.WriteJSON(map[string]any{"id": evaluate.ID, "error": map[string]any{"message": "Session with given id not found"}}); err != nil {
		t.Fatal(err)
	}

	targets := nextRemoteCaptchaTestCommand(t, browser)
	if targets.Method != "Target.getTargets" || targets.SessionID != "" {
		t.Fatalf("reattach getTargets=%+v, want browser-level Target.getTargets", targets)
	}
	if err := browser.WriteJSON(map[string]any{"id": targets.ID, "result": map[string]any{"targetInfos": []map[string]any{{
		"targetId": "authenticate-page", "type": "page", "url": "https://authenticate.riotgames.com/",
	}}}}); err != nil {
		t.Fatal(err)
	}

	attach := nextRemoteCaptchaTestCommand(t, browser)
	if attach.Method != "Target.attachToTarget" || attach.SessionID != "" {
		t.Fatalf("reattach attach=%+v, want browser-level Target.attachToTarget", attach)
	}
	if err := browser.WriteJSON(map[string]any{"id": attach.ID, "result": map[string]any{"sessionId": "fresh-session"}}); err != nil {
		t.Fatal(err)
	}

	retry := nextRemoteCaptchaTestCommand(t, browser)
	if retry.Method != "Runtime.evaluate" || retry.SessionID != "fresh-session" {
		t.Fatalf("retry evaluate=%+v, want fresh-session Runtime.evaluate", retry)
	}
	if err := browser.WriteJSON(map[string]any{"id": retry.ID, "result": map[string]any{"value": "ok"}}); err != nil {
		t.Fatal(err)
	}

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("reattached Call failed: %v", result.err)
		}
		if result.value != "ok" {
			t.Fatalf("reattached value=%q", result.value)
		}
	case <-ctx.Done():
		t.Fatal("timed out reattaching a lost Chrome page session")
	}
	if got := client.currentSessionID(); got != "fresh-session" {
		t.Fatalf("session after recover=%q, want fresh-session", got)
	}
}

func TestChromeDevToolsClientCorrelatesConcurrentOutOfOrderReplies(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)

	type callResult struct {
		method string
		value  string
		err    error
	}
	results := make(chan callResult, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, method := range []string{"Runtime.first", "Runtime.second"} {
		method := method
		go func() {
			var result struct {
				Value string `json:"value"`
			}
			err := client.Call(ctx, method, map[string]any{}, &result)
			results <- callResult{method: method, value: result.Value, err: err}
		}()
	}

	type command struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	commands := make([]command, 0, 2)
	for len(commands) < 2 {
		var command command
		if err := browser.ReadJSON(&command); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	for i := len(commands) - 1; i >= 0; i-- {
		command := commands[i]
		if err := browser.WriteJSON(map[string]any{
			"id": command.ID, "result": map[string]any{"value": command.Method},
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := make(map[string]string, 2)
	for range commands {
		result := <-results
		if result.err != nil {
			t.Fatalf("%s: %v", result.method, result.err)
		}
		got[result.method] = result.value
	}
	for _, method := range []string{"Runtime.first", "Runtime.second"} {
		if got[method] != method {
			t.Fatalf("%s result = %q, want correlated response", method, got[method])
		}
	}
}

func TestChromeDevToolsClientFansOutFilteredEventsToIndependentSubscribers(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	networkA, err := client.SubscribeEvents("riot-session",
		"Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished")
	if err != nil {
		t.Fatal(err)
	}
	defer networkA.Close()
	networkB, err := client.SubscribeEvents("riot-session", "Network.responseReceived")
	if err != nil {
		t.Fatal(err)
	}
	defer networkB.Close()
	page, err := client.SubscribeEvents("riot-session", "Page.screencastFrame")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Close()

	for _, event := range []map[string]any{
		{"method": "Network.responseReceived", "sessionId": "other-session", "params": map[string]any{"requestId": "wrong-session"}},
		{"method": "Runtime.consoleAPICalled", "sessionId": "riot-session", "params": map[string]any{"type": "log"}},
		{"method": "Network.responseReceived", "sessionId": "riot-session", "params": map[string]any{"requestId": "network-1"}},
		{"method": "Page.screencastFrame", "sessionId": "riot-session", "params": map[string]any{"sessionId": 7}},
	} {
		if err := browser.WriteJSON(event); err != nil {
			t.Fatal(err)
		}
	}

	for name, subscription := range map[string]*chromeDevToolsEventSubscription{
		"network A": networkA,
		"network B": networkB,
	} {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if event.Method != "Network.responseReceived" || !strings.Contains(string(event.Params), "network-1") {
			t.Fatalf("%s event = %+v, want matching network event", name, event)
		}
	}
	pageEvent, err := page.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pageEvent.Method != "Page.screencastFrame" {
		t.Fatalf("page event = %+v, want filtered screencast event", pageEvent)
	}

	networkA.Close()
	if _, err := networkA.Next(context.Background()); !errors.Is(err, errChromeDevToolsEventSubscriptionClosed) {
		t.Fatalf("unsubscribed Next error = %v, want subscription closed", err)
	}
	if err := browser.WriteJSON(map[string]any{
		"method": "Network.responseReceived", "sessionId": "riot-session",
		"params": map[string]any{"requestId": "network-2"},
	}); err != nil {
		t.Fatal(err)
	}
	event, err := networkB.Next(ctx)
	if err != nil || !strings.Contains(string(event.Params), "network-2") {
		t.Fatalf("remaining subscriber event/error = %+v/%v", event, err)
	}
}

func TestChromeDevToolsClientCancellationDoesNotPoisonLaterCalls(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	transport := &signalingChromeDevToolsTransport{
		chromeDevToolsPipe: host,
		writeStarted:       make(chan struct{}),
		writeFinished:      make(chan struct{}),
	}
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(transport)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- client.Call(ctx, "Runtime.canceled", map[string]any{}, nil) }()
	var first struct {
		ID int64 `json:"id"`
	}
	if err := browser.ReadJSON(&first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.writeFinished:
	case <-time.After(time.Second):
		t.Fatal("first command was read but its private-pipe write did not finish")
	}
	cancel()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled call error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled call stayed blocked")
	}
	if err := browser.WriteJSON(map[string]any{"id": first.ID, "result": map[string]any{"late": true}}); err != nil {
		t.Fatal(err)
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- client.Call(context.Background(), "Runtime.afterCancel", map[string]any{}, nil) }()
	var second struct {
		ID int64 `json:"id"`
	}
	if err := browser.ReadJSON(&second); err != nil {
		t.Fatal(err)
	}
	if err := browser.WriteJSON(map[string]any{"id": second.ID, "result": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("late response for canceled call poisoned the next call")
	}
}

func TestChromeDevToolsClientIgnoresDuplicateResponseID(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)

	firstDone := make(chan error, 1)
	go func() { firstDone <- client.Call(context.Background(), "Runtime.first", map[string]any{}, nil) }()
	var first struct {
		ID int64 `json:"id"`
	}
	if err := browser.ReadJSON(&first); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := browser.WriteJSON(map[string]any{"id": first.ID, "result": map[string]any{}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- client.Call(context.Background(), "Runtime.second", map[string]any{}, nil) }()
	var second struct {
		ID int64 `json:"id"`
	}
	if err := browser.ReadJSON(&second); err != nil {
		t.Fatal(err)
	}
	if err := browser.WriteJSON(map[string]any{"id": second.ID, "result": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate response ID disrupted a later call")
	}
}

func TestChromeDevToolsClientMalformedFrameTerminatesAllWaiters(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	done := make(chan error, 1)
	go func() { done <- client.Call(context.Background(), "Runtime.evaluate", map[string]any{}, nil) }()
	var command map[string]any
	if err := browser.ReadJSON(&command); err != nil {
		t.Fatal(err)
	}
	if _, err := browser.writer.Write([]byte(`{"secret":"must-not-be-logged",}` + "\x00")); err != nil {
		t.Fatal(err)
	}
	var terminal error
	select {
	case terminal = <-done:
		if !errors.Is(terminal, errChromeDevToolsClientClosed) {
			t.Fatalf("malformed-frame error = %v, want stable terminal error", terminal)
		}
		if strings.Contains(terminal.Error(), "must-not-be-logged") {
			t.Fatalf("malformed-frame error exposed raw payload: %v", terminal)
		}
	case <-time.After(time.Second):
		t.Fatal("malformed frame did not unblock pending call")
	}
	if err := client.Call(context.Background(), "Runtime.afterMalformed", map[string]any{}, nil); err.Error() != terminal.Error() {
		t.Fatalf("call after malformed frame error = %v, want stable %v", err, terminal)
	}
}

func TestChromeDevToolsClientPipeCloseUnblocksEveryWaiter(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- client.Call(context.Background(), "Runtime.pending", map[string]any{}, nil) }()
	}
	for i := 0; i < 2; i++ {
		var command map[string]any
		if err := browser.ReadJSON(&command); err != nil {
			t.Fatal(err)
		}
	}
	if err := browser.Close(); err != nil {
		t.Fatal(err)
	}
	var first string
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if !errors.Is(err, errChromeDevToolsClientClosed) {
				t.Fatalf("waiter error = %v, want stable terminal error", err)
			}
			if i == 0 {
				first = err.Error()
			} else if err.Error() != first {
				t.Fatalf("waiter terminal errors differ: %q and %q", first, err)
			}
		case <-time.After(time.Second):
			t.Fatal("pipe close did not unblock every waiter")
		}
	}
	if err := client.Call(context.Background(), "Runtime.pending", map[string]any{}, nil); err.Error() != first {
		t.Fatalf("call after pipe close error = %v, want stable %q", err, first)
	}
}

func TestChromeDevToolsClientSubscriberOverflowIsExplicitAndPreservesQueuedOrder(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	network, err := client.subscribeEvents("riot-session", 3,
		"Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished", "Network.dataReceived")
	if err != nil {
		t.Fatal(err)
	}
	defer network.Close()
	page, err := client.SubscribeEvents("riot-session", "Page.screencastFrame")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Close()

	for _, event := range []map[string]any{
		{"method": "Network.requestWillBeSent", "sessionId": "other-session"},
		{"method": "Page.screencastFrame", "sessionId": "riot-session", "params": map[string]any{"sessionId": 9}},
		{"method": "Network.requestWillBeSent", "sessionId": "riot-session", "params": map[string]any{"requestId": "login"}},
		{"method": "Network.responseReceived", "sessionId": "riot-session", "params": map[string]any{"requestId": "login"}},
		{"method": "Network.loadingFinished", "sessionId": "riot-session", "params": map[string]any{"requestId": "login"}},
		{"method": "Network.dataReceived", "sessionId": "riot-session", "params": map[string]any{"requestId": "overflow"}},
	} {
		if err := browser.WriteJSON(event); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-network.Done():
	case <-time.After(time.Second):
		t.Fatal("overflow did not terminate the bounded subscriber")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, method := range []string{"Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished"} {
		event, err := network.Next(ctx)
		if err != nil {
			t.Fatalf("ordered %s event: %v", method, err)
		}
		if event.Method != method || !strings.Contains(string(event.Params), "login") {
			t.Fatalf("ordered event = %+v, want %s for login", event, method)
		}
	}
	if _, err := network.Next(ctx); !errors.Is(err, errChromeDevToolsEventOverflow) {
		t.Fatalf("overflow terminal error = %v, want explicit overflow", err)
	}
	pageEvent, err := page.Next(ctx)
	if err != nil || pageEvent.Method != "Page.screencastFrame" {
		t.Fatalf("independent page subscriber event/error = %+v/%v", pageEvent, err)
	}

	done := make(chan error, 1)
	go func() { done <- client.Call(context.Background(), "Runtime.afterEvents", map[string]any{}, nil) }()
	var command struct {
		ID int64 `json:"id"`
	}
	if err := browser.ReadJSON(&command); err != nil {
		t.Fatal(err)
	}
	if err := browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("overflowed subscriber blocked response dispatch")
	}
}

type signalingChromeDevToolsTransport struct {
	*chromeDevToolsPipe
	writeStarted  chan struct{}
	writeFinished chan struct{}
	startOnce     sync.Once
	finishOnce    sync.Once
}

func (t *signalingChromeDevToolsTransport) WriteJSON(value any) error {
	t.startOnce.Do(func() { close(t.writeStarted) })
	err := t.chromeDevToolsPipe.WriteJSON(value)
	if t.writeFinished != nil {
		t.finishOnce.Do(func() { close(t.writeFinished) })
	}
	return err
}

func TestChromeDevToolsClientCancellationInterruptsBlockedPrivatePipeWrite(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	transport := &signalingChromeDevToolsTransport{chromeDevToolsPipe: host, writeStarted: make(chan struct{})}
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(transport)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- client.Call(ctx, "Runtime.blockedWrite", map[string]any{}, nil) }()
	select {
	case <-transport.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("call did not enter the production private-pipe write")
	}
	cancel()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked-write cancellation error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not interrupt blocked private-pipe write")
	}

	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending calls after blocked-write cancellation = %d, want 0", pending)
	}
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- client.Call(context.Background(), "Runtime.afterBlockedWrite", map[string]any{}, nil)
	}()
	select {
	case err := <-secondDone:
		if !errors.Is(err, errChromeDevToolsClientClosed) {
			t.Fatalf("later writer error = %v, want stable terminal error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled blocked write stalled a later writer")
	}
}

type barrierChromeDevToolsTransport struct {
	writeStarted chan struct{}
	releaseWrite chan struct{}
	closed       chan struct{}
	startOnce    sync.Once
	closeOnce    sync.Once
}

func newBarrierChromeDevToolsTransport() *barrierChromeDevToolsTransport {
	return &barrierChromeDevToolsTransport{
		writeStarted: make(chan struct{}),
		releaseWrite: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (t *barrierChromeDevToolsTransport) ReadJSON(any) error {
	<-t.closed
	return errors.New("barrier reader closed")
}

func (t *barrierChromeDevToolsTransport) WriteJSON(any) error {
	t.startOnce.Do(func() { close(t.writeStarted) })
	<-t.releaseWrite
	return errors.New("barrier write failed")
}

func (t *barrierChromeDevToolsTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

func TestChromeDevToolsClientCloseAndWriteFailureShareOneTerminalError(t *testing.T) {
	transport := newBarrierChromeDevToolsTransport()
	client := newChromeDevToolsClient(transport)
	calls := make(chan error, 2)
	go func() { calls <- client.Call(context.Background(), "Runtime.first", map[string]any{}, nil) }()
	go func() { calls <- client.Call(context.Background(), "Runtime.second", map[string]any{}, nil) }()
	select {
	case <-transport.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write did not reach barrier")
	}

	closeDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { closeDone <- client.Close(ctx) }()
	close(transport.releaseWrite)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close stayed blocked during write failure")
	}

	var terminal string
	for i := 0; i < 2; i++ {
		select {
		case err := <-calls:
			if !errors.Is(err, errChromeDevToolsClientClosed) {
				t.Fatalf("in-flight call error = %v, want stable terminal", err)
			}
			if i == 0 {
				terminal = err.Error()
			} else if err.Error() != terminal {
				t.Fatalf("in-flight terminal errors differ: %q and %q", terminal, err)
			}
		case <-time.After(time.Second):
			t.Fatal("in-flight call stayed blocked")
		}
	}
	if err := client.Call(context.Background(), "Runtime.later", map[string]any{}, nil); err.Error() != terminal {
		t.Fatalf("later call error = %v, want stable %q", err, terminal)
	}
}

type countingChromeDevToolsTransport struct {
	chromeDevToolsTransport
	active atomic.Int32
	max    atomic.Int32
}

func (t *countingChromeDevToolsTransport) ReadJSON(value any) error {
	active := t.active.Add(1)
	defer t.active.Add(-1)
	for {
		maximum := t.max.Load()
		if active <= maximum || t.max.CompareAndSwap(maximum, active) {
			break
		}
	}
	return t.chromeDevToolsTransport.ReadJSON(value)
}

func TestChromeDevToolsClientUsesExactlyOnePipeReader(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	counted := &countingChromeDevToolsTransport{chromeDevToolsTransport: host}
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(counted)
	done := make(chan error, 8)
	for i := 0; i < cap(done); i++ {
		go func() { done <- client.Call(context.Background(), "Runtime.concurrent", map[string]any{}, nil) }()
	}
	for i := 0; i < cap(done); i++ {
		var command struct {
			ID int64 `json:"id"`
		}
		if err := browser.ReadJSON(&command); err != nil {
			t.Fatal(err)
		}
		if err := browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < cap(done); i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := counted.max.Load(); got != 1 {
		t.Fatalf("maximum concurrent pipe readers = %d, want exactly 1", got)
	}
}

func TestChromeDevToolsClientCloseStopsReaderWithoutReadablePeer(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() { _ = browser.Close() })
	client := newChromeDevToolsClient(host)
	subscription, err := client.SubscribeEvents("", "Page.screencastFrame")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.Next(context.Background()); !errors.Is(err, errChromeDevToolsClientClosed) {
		t.Fatalf("subscription error after client close = %v, want stable terminal", err)
	}
	if err := client.Call(context.Background(), "Runtime.afterClose", map[string]any{}, nil); !errors.Is(err, errChromeDevToolsClientClosed) {
		t.Fatalf("call after close error = %v, want stable terminal error", err)
	}
}
