package authweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const chromeDevToolsEventBuffer = 256
const chromeDevToolsWriteCancelGrace = 10 * time.Millisecond

var (
	errChromeDevToolsClientClosed            = errors.New("Riot Chrome DevTools client closed")
	errChromeDevToolsEventSubscriptionClosed = errors.New("Riot Chrome DevTools event subscription closed")
	errChromeDevToolsEventOverflow           = errors.New("Riot Chrome DevTools event subscription overflow")
)

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

func lostChromeDevToolsSession(err error) bool {
	var protocolErr *chromeDevToolsProtocolError
	if !errors.As(err, &protocolErr) {
		return false
	}
	return strings.Contains(strings.ToLower(protocolErr.Message), "session with given id not found")
}

func isBrowserLevelChromeDevToolsMethod(method string) bool {
	return strings.HasPrefix(method, "Target.")
}

type chromeDevToolsResponse struct {
	message chromeDevToolsMessage
	err     error
}

type chromeDevToolsWriteRequest struct {
	ctx     context.Context
	command map[string]any
	done    chan error
}

type chromeDevToolsEventSubscription struct {
	client *chromeDevToolsClient
	id     uint64
	events chan chromeDevToolsMessage
	done   chan struct{}
	filter chromeDevToolsEventFilter

	finishOnce sync.Once
	mu         sync.Mutex
	terminal   error
}

type chromeDevToolsEventFilter struct {
	sessionID string
	methods   map[string]struct{}
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

	nextSubscriberID uint64
	subscribers      map[uint64]*chromeDevToolsEventSubscription
	done             chan struct{}
	closedSignal     chan struct{}

	recoverMu sync.Mutex

	writesMu    sync.Mutex
	activeWrite *chromeDevToolsWriteRequest
	writeQueue  chan *chromeDevToolsWriteRequest

	transportCloseOnce sync.Once
	transportClosed    chan struct{}
	transportCloseErr  error
}

func newChromeDevToolsClient(conn chromeDevToolsTransport) *chromeDevToolsClient {
	client := &chromeDevToolsClient{conn: conn}
	client.start()
	return client
}

func (c *chromeDevToolsClient) start() {
	c.startOnce.Do(func() {
		c.pending = make(map[int64]chan chromeDevToolsResponse)
		c.subscribers = make(map[uint64]*chromeDevToolsEventSubscription)
		c.done = make(chan struct{})
		c.closedSignal = make(chan struct{})
		c.writeQueue = make(chan *chromeDevToolsWriteRequest)
		c.transportClosed = make(chan struct{})
		go c.readLoop()
		go c.writeLoop()
	})
}

func (c *chromeDevToolsClient) Call(ctx context.Context, method string, params any, result any) error {
	useSession := !isBrowserLevelChromeDevToolsMethod(method)
	err := c.send(ctx, method, params, result, useSession)
	if err == nil || !useSession || !lostChromeDevToolsSession(err) {
		return err
	}
	if recErr := c.recoverRiotPageSession(ctx); recErr != nil {
		if errors.Is(recErr, errRiotCaptchaSurfaceUnavailable) || errors.Is(recErr, errRiotCaptchaDocumentChanged) {
			return recErr
		}
		return err
	}
	return c.send(ctx, method, params, result, true)
}

func (c *chromeDevToolsClient) send(ctx context.Context, method string, params any, result any, useSession bool) error {
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
	sessionID := ""
	if useSession {
		sessionID = c.sessionID
	}
	c.pending[id] = response
	c.mu.Unlock()

	command := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		command["sessionId"] = sessionID
	}
	writeRequest := &chromeDevToolsWriteRequest{
		ctx: ctx, command: command, done: make(chan error, 1),
	}
	select {
	case c.writeQueue <- writeRequest:
	case <-ctx.Done():
		c.removePending(id, response)
		return fmt.Errorf("Riot Chrome %s: %w", method, ctx.Err())
	case <-c.closedSignal:
		return c.terminalError()
	}

	select {
	case err := <-writeRequest.done:
		if ctxErr := ctx.Err(); ctxErr != nil {
			c.removePending(id, response)
			return fmt.Errorf("Riot Chrome %s: %w", method, ctxErr)
		}
		if err != nil {
			return err
		}
	case <-ctx.Done():
		c.removePending(id, response)
		c.abortWrite(writeRequest)
		return fmt.Errorf("Riot Chrome %s: %w", method, ctx.Err())
	case <-c.closedSignal:
		if ctxErr := ctx.Err(); ctxErr != nil {
			c.removePending(id, response)
			c.abortWrite(writeRequest)
			return fmt.Errorf("Riot Chrome %s: %w", method, ctxErr)
		}
		return c.terminalError()
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
		if c.removePending(id, response) {
			return fmt.Errorf("Riot Chrome %s: %w", method, ctx.Err())
		}
		reply := <-response
		if reply.err != nil {
			return reply.err
		}
		return fmt.Errorf("Riot Chrome %s: %w", method, ctx.Err())
	case <-c.closedSignal:
		return c.terminalError()
	}
}

func (c *chromeDevToolsClient) removePending(id int64, response chan chromeDevToolsResponse) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending[id] != response {
		return false
	}
	delete(c.pending, id)
	return true
}

func (c *chromeDevToolsClient) abortWrite(request *chromeDevToolsWriteRequest) {
	timer := time.NewTimer(chromeDevToolsWriteCancelGrace)
	defer timer.Stop()
	select {
	case <-request.done:
		return
	case <-timer.C:
	case <-c.closedSignal:
		return
	}
	c.writesMu.Lock()
	active := c.activeWrite == request
	c.writesMu.Unlock()
	if !active {
		return
	}
	c.terminate(fmt.Errorf("%w: canceled blocked write", errChromeDevToolsClientClosed))
	c.shutdownTransport()
}

// call preserves the package-private synchronous helper while all pipe reads
// are owned by the dispatcher's single reader goroutine.
func (c *chromeDevToolsClient) call(method string, params any, result any) error {
	return c.Call(context.Background(), method, params, result)
}

func (c *chromeDevToolsClient) SubscribeEvents(sessionID string, methods ...string) (*chromeDevToolsEventSubscription, error) {
	return c.subscribeEvents(sessionID, chromeDevToolsEventBuffer, methods...)
}

func (c *chromeDevToolsClient) subscribeEvents(sessionID string, buffer int, methods ...string) (*chromeDevToolsEventSubscription, error) {
	if c == nil || c.conn == nil {
		return nil, errChromeDevToolsClientClosed
	}
	c.start()
	if buffer <= 0 {
		return nil, errors.New("Riot Chrome DevTools event subscription buffer must be positive")
	}
	methodSet := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		if method != "" {
			methodSet[method] = struct{}{}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, c.terminal
	}
	c.nextSubscriberID++
	subscription := &chromeDevToolsEventSubscription{
		client: c,
		id:     c.nextSubscriberID,
		events: make(chan chromeDevToolsMessage, buffer),
		done:   make(chan struct{}),
		filter: chromeDevToolsEventFilter{sessionID: sessionID, methods: methodSet},
	}
	c.subscribers[subscription.id] = subscription
	return subscription, nil
}

func (s *chromeDevToolsEventSubscription) Next(ctx context.Context) (chromeDevToolsMessage, error) {
	if s == nil {
		return chromeDevToolsMessage{}, errChromeDevToolsEventSubscriptionClosed
	}
	if err := ctx.Err(); err != nil {
		return chromeDevToolsMessage{}, err
	}
	select {
	case <-ctx.Done():
		return chromeDevToolsMessage{}, ctx.Err()
	case event, ok := <-s.events:
		if !ok {
			return chromeDevToolsMessage{}, s.terminalError()
		}
		if err := ctx.Err(); err != nil {
			return chromeDevToolsMessage{}, err
		}
		return event, nil
	}
}

func (s *chromeDevToolsEventSubscription) Close() {
	if s == nil {
		return
	}
	if s.client == nil {
		s.finish(errChromeDevToolsEventSubscriptionClosed)
		return
	}
	s.client.unsubscribe(s, errChromeDevToolsEventSubscriptionClosed)
}

func (s *chromeDevToolsEventSubscription) Done() <-chan struct{} {
	if s == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return s.done
}

func (s *chromeDevToolsEventSubscription) matches(message chromeDevToolsMessage) bool {
	if s.filter.sessionID != "" && message.SessionID != s.filter.sessionID {
		return false
	}
	if len(s.filter.methods) == 0 {
		return true
	}
	_, ok := s.filter.methods[message.Method]
	return ok
}

func (s *chromeDevToolsEventSubscription) finish(err error) {
	s.finishOnce.Do(func() {
		if err == nil {
			err = errChromeDevToolsEventSubscriptionClosed
		}
		s.mu.Lock()
		s.terminal = err
		s.mu.Unlock()
		close(s.events)
		close(s.done)
	})
}

func (s *chromeDevToolsEventSubscription) terminalError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal != nil {
		return s.terminal
	}
	return errChromeDevToolsEventSubscriptionClosed
}

func (c *chromeDevToolsClient) unsubscribe(subscription *chromeDevToolsEventSubscription, terminal error) {
	c.mu.Lock()
	if c.subscribers[subscription.id] == subscription {
		delete(c.subscribers, subscription.id)
	}
	c.mu.Unlock()
	subscription.finish(terminal)
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

func (c *chromeDevToolsClient) writeLoop() {
	for {
		select {
		case <-c.closedSignal:
			return
		case request := <-c.writeQueue:
			if err := request.ctx.Err(); err != nil {
				request.done <- err
				continue
			}
			select {
			case <-c.closedSignal:
				request.done <- c.terminalError()
				return
			default:
			}
			c.writesMu.Lock()
			if err := request.ctx.Err(); err != nil {
				c.writesMu.Unlock()
				request.done <- err
				continue
			}
			c.activeWrite = request
			c.writesMu.Unlock()

			err := c.conn.WriteJSON(request.command)

			c.writesMu.Lock()
			if c.activeWrite == request {
				c.activeWrite = nil
			}
			c.writesMu.Unlock()
			if err != nil {
				terminal := c.terminate(fmt.Errorf("%w: write command: %v", errChromeDevToolsClientClosed, err))
				request.done <- terminal
				c.shutdownTransport()
				return
			}
			request.done <- nil
		}
	}
}

func (c *chromeDevToolsClient) readLoop() {
	defer close(c.done)
	for {
		var message chromeDevToolsMessage
		if err := c.conn.ReadJSON(&message); err != nil {
			c.terminate(fmt.Errorf("%w: read response: %v", errChromeDevToolsClientClosed, err))
			c.shutdownTransport()
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
		c.dispatchEvent(message)
	}
}

func (c *chromeDevToolsClient) dispatchEvent(message chromeDevToolsMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, subscription := range c.subscribers {
		if !subscription.matches(message) {
			continue
		}
		select {
		case subscription.events <- message:
		default:
			delete(c.subscribers, id)
			subscription.finish(errChromeDevToolsEventOverflow)
		}
	}
}

func (c *chromeDevToolsClient) terminate(err error) error {
	if err == nil {
		err = errChromeDevToolsClientClosed
	}
	c.mu.Lock()
	if c.closed {
		terminal := c.terminal
		c.mu.Unlock()
		return terminal
	}
	c.closed = true
	c.terminal = err
	close(c.closedSignal)
	pending := c.pending
	c.pending = make(map[int64]chan chromeDevToolsResponse)
	for _, response := range pending {
		response <- chromeDevToolsResponse{err: err}
	}
	subscribers := c.subscribers
	c.subscribers = make(map[uint64]*chromeDevToolsEventSubscription)
	for _, subscription := range subscribers {
		subscription.finish(err)
	}
	c.mu.Unlock()
	return err
}

func closeChromeDevToolsTransport(transport chromeDevToolsTransport) error {
	closer, ok := transport.(interface{ Close() error })
	if !ok {
		return nil
	}
	return closer.Close()
}

func (c *chromeDevToolsClient) shutdownTransport() {
	c.transportCloseOnce.Do(func() {
		go func() {
			err := closeChromeDevToolsTransport(c.conn)
			c.mu.Lock()
			c.transportCloseErr = err
			c.mu.Unlock()
			close(c.transportClosed)
		}()
	})
}

func (c *chromeDevToolsClient) Close(ctx context.Context) error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.start()
	c.terminate(errChromeDevToolsClientClosed)
	c.shutdownTransport()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.transportClosed:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.transportCloseErr
}
