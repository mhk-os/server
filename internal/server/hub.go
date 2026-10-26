package server

import (
	"sync"

	"github.com/mhk-os/server/internal/proto"
)

// Hub tracks live connections, all of them and the logged-in ones by account id, so any part of the server (later:
// moderation, lobbies) can push to a player or drop them.
type Hub struct {
	mu        sync.RWMutex
	all       map[*Conn]struct{}
	byAccount map[uint64]map[*Conn]struct{}
}

func newHub() *Hub {
	return &Hub{all: map[*Conn]struct{}{}, byAccount: map[uint64]map[*Conn]struct{}{}}
}

func (h *Hub) add(c *Conn) {
	h.mu.Lock()
	h.all[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *Conn) {
	h.mu.Lock()
	delete(h.all, c)
	h.mu.Unlock()
}

func (h *Hub) bind(c *Conn, accountID uint64) {
	h.mu.Lock()
	set := h.byAccount[accountID]
	if set == nil {
		set = map[*Conn]struct{}{}
		h.byAccount[accountID] = set
	}
	set[c] = struct{}{}
	h.mu.Unlock()
}

// unbind forgets c as a connection of accountID and reports whether it was the account's last one.
func (h *Hub) unbind(c *Conn, accountID uint64) (last bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.byAccount[accountID]
	if _, ok := set[c]; !ok {
		return false
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.byAccount, accountID)
		return true
	}
	return false
}

// Conns returns a snapshot of all live connections.
func (h *Hub) Conns() []*Conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Conn, 0, len(h.all))
	for c := range h.all {
		out = append(out, c)
	}
	return out
}

// AccountConns returns a snapshot of the connections logged in as accountID (a player may be online twice).
func (h *Hub) AccountConns(accountID uint64) []*Conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	set := h.byAccount[accountID]
	out := make([]*Conn, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	return out
}

// Online reports whether accountID has at least one logged-in connection.
func (h *Hub) Online(accountID uint64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byAccount[accountID]) > 0
}

// Push sends a push message to every connection of accountID and returns how many were reached.
func (h *Hub) Push(accountID uint64, typ proto.Type, m proto.Message) int {
	conns := h.AccountConns(accountID)
	body := proto.Encode(m)
	for _, c := range conns {
		c.pushBody(typ, body)
	}
	return len(conns)
}

// Disconnect pushes Disconnect(status, message) to every connection of accountID and closes them.
func (h *Hub) Disconnect(accountID uint64, status proto.Status, message string) int {
	conns := h.AccountConns(accountID)
	for _, c := range conns {
		c.Disconnect(status, message)
	}
	return len(conns)
}
