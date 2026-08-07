package bot

import (
	"context"
	"time"
)

// Shutdown cancels authentication watchers and waits for them to leave before
// the Discord session and backing stores are closed. It is idempotent.
func (h *Handlers) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	h.lifecycleMu.Lock()
	if h.lifecycleDone == nil {
		h.lifecycleDone = make(chan struct{})
	}
	done := h.lifecycleDone
	if !h.lifecycleClosed {
		h.lifecycleClosed = true
		if h.lifecycleCancel != nil {
			h.lifecycleCancel()
		}
	}
	h.lifecycleMu.Unlock()

	h.lifecycleWaitOnce.Do(func() {
		go func() {
			h.lifecycleWG.Wait()
			h.captchaWatchMu.Lock()
			clear(h.captchaWatches)
			h.captchaWatchMu.Unlock()
			h.captchaEditMu.Lock()
			clear(h.captchaEdits)
			h.captchaEditMu.Unlock()
			h.mfaHintMu.Lock()
			clear(h.mfaHints)
			h.mfaHintMu.Unlock()
			h.mfaSubmitMu.Lock()
			clear(h.mfaSubmitGuards)
			h.mfaSubmitMu.Unlock()
			close(done)
		}()
	})
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Handlers) beginLifecycleWorker(timeout time.Duration) (context.Context, func(), bool) {
	h.lifecycleMu.Lock()
	if h.lifecycleClosed {
		h.lifecycleMu.Unlock()
		return nil, nil, false
	}
	if h.lifecycleCtx == nil {
		h.lifecycleCtx, h.lifecycleCancel = context.WithCancel(context.Background())
	}
	root := h.lifecycleCtx
	h.lifecycleWG.Add(1)
	h.lifecycleMu.Unlock()
	ctx, cancel := context.WithTimeout(root, timeout)
	return ctx, func() {
		cancel()
		h.lifecycleWG.Done()
	}, true
}

func (h *Handlers) captchaEditGuard(state string) *captchaEditGuard {
	h.captchaEditMu.Lock()
	if h.captchaEdits == nil {
		h.captchaEdits = make(map[string]*captchaEditGuard)
	}
	guard := h.captchaEdits[state]
	created := false
	if guard == nil {
		guard = &captchaEditGuard{}
		h.captchaEdits[state] = guard
		created = true
	}
	h.captchaEditMu.Unlock()
	if created {
		ctx, done, ok := h.beginLifecycleWorker(passwordCaptchaTimeout + time.Minute)
		if ok {
			go func() {
				defer done()
				<-ctx.Done()
				h.captchaEditMu.Lock()
				if h.captchaEdits[state] == guard {
					delete(h.captchaEdits, state)
				}
				h.captchaEditMu.Unlock()
			}()
		}
	}
	return guard
}
