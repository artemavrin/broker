// Package wshub maintains the registry of live WebSocket sessions keyed by
// participant id. A single participant may hold several concurrent sessions;
// every one receives the doorbell when a message is enqueued for it.
package wshub

import "sync"

// Session represents one live WS connection. The owner reads Signal to learn
// when to fetch; the hub writes to it. The signal is coalescing: it only ever
// conveys "there might be new work", never how much.
type Session struct {
	id          uint64
	participant string
	signal      chan struct{}
}

// Signal returns the channel the session owner selects on to receive
// doorbells. A receive means "fetch now".
func (s *Session) Signal() <-chan struct{} { return s.signal }

// Hub is a concurrency-safe registry of sessions grouped by participant id.
type Hub struct {
	mu       sync.Mutex
	sessions map[string]map[uint64]*Session
	nextID   uint64
}

// New creates an empty Hub.
func New() *Hub {
	return &Hub{sessions: make(map[string]map[uint64]*Session)}
}

// Add registers a new session for participant and returns it.
func (h *Hub) Add(participant string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	s := &Session{
		id:          h.nextID,
		participant: participant,
		signal:      make(chan struct{}, 1),
	}
	m := h.sessions[participant]
	if m == nil {
		m = make(map[uint64]*Session)
		h.sessions[participant] = m
	}
	m[s.id] = s
	return s
}

// Remove deregisters a session. Safe to call more than once.
func (h *Hub) Remove(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.sessions[s.participant]
	if m == nil {
		return
	}
	delete(m, s.id)
	if len(m) == 0 {
		delete(h.sessions, s.participant)
	}
}

// Notify rings the doorbell for every session belonging to participant. The
// send is non-blocking: if a session already has a pending signal it is left
// as is (coalesced), since one fetch drains all available work.
func (h *Hub) Notify(participant string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions[participant] {
		select {
		case s.signal <- struct{}{}:
		default:
		}
	}
}
