package session

import "sync"

// A remote close may arrive before the client finishes installing its hook.
// Remember closure and execute a late-installed hook exactly once.
type closeHook struct {
	mu    sync.Mutex
	fired bool
	hook  func()
}

func (h *closeHook) set(fn func()) {
	h.mu.Lock()
	if h.fired {
		h.mu.Unlock()
		fn()
		return
	}
	h.hook = fn
	h.mu.Unlock()
}
func (h *closeHook) run() {
	h.mu.Lock()
	if h.fired {
		h.mu.Unlock()
		return
	}
	h.fired = true
	fn := h.hook
	h.hook = nil
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
}
