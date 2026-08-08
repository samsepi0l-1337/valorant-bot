package authweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

const chromeDevToolsEventBuffer = 256

var errChromeDevToolsClientClosed = errors.New("Riot Chrome DevTools client closed")

type chromeDevToolsMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type chromeDevToolsProtocolError struct {
	Method  string
	Message string
}

func (e *chromeDevToolsProtocolError) Error() string {
	return fmt.Sprintf("Riot Chrome %s: %s", e.Method, e.Message)
}

type chromeDevToolsResponse struct {
	message chromeDevToolsMessage
	err     error
}

type chromeDevToolsClient struct {
	conn chromeDevToolsTransport

	startOnce sync.Once
	mu        sync.Mutex
	nextID    int64
	pending   map[int64]chan chromeDevToolsResponse
	sessionID string
	closed    bool
	terminal  error
	events    chan chromeDevToolsMessage
	done      chan struct{}

	closeOnce     sync.Once
	closeFinished chan struct{}
	closeErr      error
}

func newChromeDevToolsClient(conn chromeDevToolsTransport) *chromeDevToolsClient {
	client := &chromeDevToolsClient{conn: conn}
	client.start()
	return client
}

func (c *chromeDevToolsClient) start() {
	c.startOnce.Do(func() {
		c.pending = make(map[int64]chan chromeDevToolsResponse)
		c.events = make(chan chromeDevToolsMessage, chromeDevToolsEventBuffer)
		c.done = make(chan struct{})
		c.closeFinished = make(chan struct{})
		go c.readLoop()
	})
}

func (c *chromeDevToolsClient) Call(ctx context.Context, method string, params any, result any) error {
	if c == nil || c.conn == nil {
		return errChromeDevToolsClientClosed
	}
	c.start()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Riot Chrome %s: %w", method, err)
	}

	response := make(chan chromeDevToolsResponse, 1)
	c.mu.Lock()
	if c.closed {
		err := c.terminal
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	sessionID := c.sessionID
	c.pending[id] = response
	c.mu.Unlock()

	command := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		command["sessionId"] = sessionID
	}
	if err := c.conn.WriteJSON(command); err != nil {
		c.mu.Lock()
		if c.pending[id] == response {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		c.failTransport(fmt.Errorf("%w: write command: %v", errChromeDevToolsClientClosed, err))
		return fmt.Errorf("Riot Chrome %s: %w", method, err)
	}

	select {
	case reply := <-response:
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("Riot Chrome %s: %w", method, err)
		}
		if reply.err != nil {
			return reply.err
		}
		if reply.message.Error != nil {
			return &chromeDevToolsProtocolError{Method: method, Message: reply.message.Error.Message}
		}
		if result != nil && len(reply.message.Result) > 0 {
			if err := json.Unmarshal(reply.message.Result, result); err != nil {
				return fmt.Errorf("Riot Chrome %s response: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		if c.pending[id] == response {
			delete(c.pending, id)
			c.mu.Unlock()
			return fmt.Errorf("Riot Chrome %s: %w", method, ctx.Err())
		}
		c.mu.Unlock()
		reply := <-response
		if reply.err != nil {
			return reply.err
		}
		return fmt.Errorf("Riot Chrome %s: %w", method, ctx.Err())
	}
}

// call preserves the package-private synchronous helper while all pipe reads
// are owned by the dispatcher's single reader goroutine.
func (c *chromeDevToolsClient) call(method string, params any, result any) error {
	return c.Call(context.Background(), method, params, result)
}

func (c *chromeDevToolsClient) Events() <-chan chromeDevToolsMessage {
	c.start()
	return c.events
}

func (c *chromeDevToolsClient) NextEvent(ctx context.Context) (chromeDevToolsMessage, error) {
	if c == nil || c.conn == nil {
		return chromeDevToolsMessage{}, errChromeDevToolsClientClosed
	}
	c.start()
	for {
		if err := ctx.Err(); err != nil {
			return chromeDevToolsMessage{}, err
		}
		select {
		case <-ctx.Done():
			return chromeDevToolsMessage{}, ctx.Err()
		case event, ok := <-c.events:
			if !ok {
				return chromeDevToolsMessage{}, c.terminalError()
			}
			if err := ctx.Err(); err != nil {
				return chromeDevToolsMessage{}, err
			}
			if sessionID := c.currentSessionID(); sessionID != "" && event.SessionID != sessionID {
				continue
			}
			return event, nil
		}
	}
}

func (c *chromeDevToolsClient) nextEvent() (chromeDevToolsMessage, error) {
	return c.NextEvent(context.Background())
}

func (c *chromeDevToolsClient) setSessionID(sessionID string) {
	c.mu.Lock()
	c.sessionID = sessionID
	c.mu.Unlock()
}

func (c *chromeDevToolsClient) currentSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *chromeDevToolsClient) terminalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminal != nil {
		return c.terminal
	}
	return errChromeDevToolsClientClosed
}

func (c *chromeDevToolsClient) readLoop() {
	defer func() {
		close(c.events)
		close(c.done)
	}()
	for {
		var message chromeDevToolsMessage
		if err := c.conn.ReadJSON(&message); err != nil {
			c.terminate(fmt.Errorf("%w: read response: %v", errChromeDevToolsClientClosed, err))
			return
		}
		if message.ID != 0 {
			c.mu.Lock()
			response := c.pending[message.ID]
			if response != nil {
				delete(c.pending, message.ID)
				response <- chromeDevToolsResponse{message: message}
			}
			c.mu.Unlock()
			continue
		}
		select {
		case c.events <- message:
		default:
			select {
			case <-c.events:
			default:
			}
			select {
			case c.events <- message:
			default:
			}
		}
	}
}

func (c *chromeDevToolsClient) terminate(err error) {
	if err == nil {
		err = errChromeDevToolsClientClosed
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.terminal = err
	pending := c.pending
	c.pending = make(map[int64]chan chromeDevToolsResponse)
	for _, response := range pending {
		response <- chromeDevToolsResponse{err: err}
	}
	c.mu.Unlock()
}

func (c *chromeDevToolsClient) failTransport(err error) {
	c.terminate(err)
	go func() { _ = closeChromeDevToolsTransport(c.conn) }()
}

func closeChromeDevToolsTransport(transport chromeDevToolsTransport) error {
	closer, ok := transport.(interface{ Close() error })
	if !ok {
		return nil
	}
	return closer.Close()
}

func (c *chromeDevToolsClient) Close(ctx context.Context) error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.start()
	c.terminate(errChromeDevToolsClientClosed)
	c.closeOnce.Do(func() {
		go func() {
			err := closeChromeDevToolsTransport(c.conn)
			c.mu.Lock()
			c.closeErr = err
			c.mu.Unlock()
			close(c.closeFinished)
		}()
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeFinished:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}
