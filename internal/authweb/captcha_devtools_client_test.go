package authweb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

func TestChromeDevToolsClientPublishesUnsolicitedEventsWhileCallIsPending(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- client.Call(ctx, "Runtime.evaluate", map[string]any{}, nil) }()
	var command struct {
		ID int64 `json:"id"`
	}
	if err := browser.ReadJSON(&command); err != nil {
		t.Fatal(err)
	}
	if err := browser.WriteJSON(map[string]any{
		"method": "Network.loadingFinished", "sessionId": "riot-session",
		"params": map[string]any{"requestId": "request-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := browser.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-client.Events():
		if event.Method != "Network.loadingFinished" || !strings.Contains(string(event.Params), "request-1") {
			t.Fatalf("event = %+v, want unsolicited Network.loadingFinished", event)
		}
	case <-time.After(time.Second):
		t.Fatal("unsolicited event was not published")
	}
}

func TestChromeDevToolsClientCancellationDoesNotPoisonLaterCalls(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- client.Call(ctx, "Runtime.canceled", map[string]any{}, nil) }()
	var first struct {
		ID int64 `json:"id"`
	}
	if err := browser.ReadJSON(&first); err != nil {
		t.Fatal(err)
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

func TestChromeDevToolsClientBoundsEventDeliveryWithoutBlockingReplies(t *testing.T) {
	host, browser := newTestChromeDevToolsPipes()
	t.Cleanup(func() {
		_ = host.Close()
		_ = browser.Close()
	})
	client := newChromeDevToolsClient(host)
	events := client.Events()
	const extra = 37
	for i := 0; i < chromeDevToolsEventBuffer+extra; i++ {
		if err := browser.WriteJSON(map[string]any{
			"method": fmt.Sprintf("Network.event.%03d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for len(events) != chromeDevToolsEventBuffer && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(events); got != chromeDevToolsEventBuffer {
		t.Fatalf("buffered events = %d, want bounded capacity %d", got, chromeDevToolsEventBuffer)
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
		t.Fatal("full event channel blocked response dispatch")
	}

	var last chromeDevToolsMessage
	for len(events) > 0 {
		last = <-events
	}
	wantLast := fmt.Sprintf("Network.event.%03d", chromeDevToolsEventBuffer+extra-1)
	if last.Method != wantLast {
		t.Fatalf("latest buffered event = %q, want %q", last.Method, wantLast)
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-client.Events():
		if ok {
			t.Fatal("event subscription remained open after close")
		}
	case <-time.After(time.Second):
		t.Fatal("client close waited for the peer to read or respond")
	}
	if err := client.Call(context.Background(), "Runtime.afterClose", map[string]any{}, nil); !errors.Is(err, errChromeDevToolsClientClosed) {
		t.Fatalf("call after close error = %v, want stable terminal error", err)
	}
}
