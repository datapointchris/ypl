package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/datapointchris/ypl/api/wire"
)

// errStoreNotOpen is what /ready reports while the server opens its store.
var errStoreNotOpen = errors.New("the store is not open yet")

// storeHold answers every request from the moment the port is bound until the
// store is open. It answers /health at once, and /ready with 503 saying why the
// store is not open, naming the server holding its lock where one does. It
// holds every other request until open hands it the API, as Knative's activator
// holds requests while a revision starts, so a caller reads a wait where it
// would read a refusal.
type storeHold struct {
	probes   http.Handler
	opened   chan struct{}
	draining chan struct{}
	heldBy   atomic.Pointer[string]

	mu       sync.Mutex
	handler  http.Handler
	drainAPI func()
	drained  bool
}

func newStoreHold() *storeHold {
	h := &storeHold{opened: make(chan struct{}), draining: make(chan struct{})}
	h.probes = routes(h.ready)
	return h
}

func (h *storeHold) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case <-h.opened:
		h.handler.ServeHTTP(w, r)
		return
	default:
	}
	if r.URL.Path == "/health" || r.URL.Path == "/ready" {
		h.probes.ServeHTTP(w, r)
		return
	}
	select {
	case <-h.opened:
		h.handler.ServeHTTP(w, r)
	case <-h.draining:
		// Nothing of the request ran, so the caller may send it again, and the
		// next server is who answers.
		w.Header().Set("Retry-After", "1")
		wire.Refuse(w, http.StatusServiceUnavailable, wire.CodeShuttingDown, "the server stopped before it opened its store, so it did nothing with this request; send it again")
	case <-r.Context().Done():
	}
}

func (h *storeHold) ready() error {
	if heldBy := h.heldBy.Load(); heldBy != nil {
		return fmt.Errorf("%w: another server holds its lock, %s", errStoreNotOpen, *heldBy)
	}
	return errStoreNotOpen
}

// waitingOn records who holds the store's lock, for the log and for /ready.
func (h *storeHold) waitingOn(heldBy string) {
	h.heldBy.Store(&heldBy)
	slog.Info("holding requests until the server holding the store exits", "held_by", heldBy)
}

// open hands every request, the held ones first, to handler, and has drain
// call drainAPI. It returns false once drain has been called, and then opens
// nothing.
func (h *storeHold) open(handler http.Handler, drainAPI func()) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.drained {
		return false
	}
	h.handler, h.drainAPI = handler, drainAPI
	close(h.opened)
	return true
}

// drain answers each request still held with 503 shutting_down, and drains the
// API where open has handed it one.
func (h *storeHold) drain() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.drained {
		return
	}
	h.drained = true
	close(h.draining)
	if h.drainAPI != nil {
		h.drainAPI()
	}
}
