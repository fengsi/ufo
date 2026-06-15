package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsHub fans typed change events out to the fleet's WebSocket connections. Events
// originate from PostgreSQL LISTEN/NOTIFY (so they reach every API instance), so no
// external relay is needed. Each connection is scoped to one fleet.
type wsHub struct {
	mu      sync.Mutex
	byFleet map[int64]map[*wsConn]bool
}

type wsConn struct {
	fleet int64
	send  chan []byte
}

func newWSHub() *wsHub { return &wsHub{byFleet: map[int64]map[*wsConn]bool{}} }

func (h *wsHub) add(c *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byFleet[c.fleet] == nil {
		h.byFleet[c.fleet] = map[*wsConn]bool{}
	}
	h.byFleet[c.fleet][c] = true
}

func (h *wsHub) remove(c *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.byFleet[c.fleet]; m != nil {
		delete(m, c)
		if len(m) == 0 {
			delete(h.byFleet, c.fleet)
		}
	}
}

func (h *wsHub) broadcast(fleet int64, msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.byFleet[fleet] {
		select {
		case c.send <- msg:
		default: // slow client: drop (the client resyncs on reconnect)
		}
	}
}

// run consumes typed ufo_changed notifications and broadcasts {"t":<kind>} to the
// changed fleet's sockets.
func (h *wsHub) run(ctx context.Context, n *Notifier) {
	sub, unsubscribe := n.Subscribe(changedChannel)
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case note := <-sub:
			var p struct {
				T     string `json:"t"`
				Fleet int64  `json:"fleet"`
			}
			if json.Unmarshal([]byte(note.Payload), &p) != nil || p.Fleet == 0 {
				continue
			}
			msg, _ := json.Marshal(map[string]string{"t": p.T})
			h.broadcast(p.Fleet, msg)
		}
	}
}

const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = 30 * time.Second
)

// wsConnect upgrades a fleet-scoped live event stream.
func (s *Server) wsConnect(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r) // membership-checked before upgrade
	if !ok {
		return
	}
	// SameSite=Lax doesn't cover the WS upgrade, so check Origin ourselves.
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return s.originAllowed(r, r.Header.Get("Origin")) },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &wsConn{fleet: wid, send: make(chan []byte, 16)}
	s.hub.add(c)

	go func() {
		ticker := time.NewTicker(wsPingPeriod)
		defer func() { ticker.Stop(); conn.Close() }()
		for {
			select {
			case msg, ok := <-c.send:
				_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
				if !ok {
					_ = conn.WriteMessage(websocket.CloseMessage, nil)
					return
				}
				if conn.WriteMessage(websocket.TextMessage, msg) != nil {
					return
				}
			case <-ticker.C:
				_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
				if conn.WriteMessage(websocket.PingMessage, nil) != nil {
					return
				}
			}
		}
	}()

	// Read pump: we don't expect client messages — just keep the pong-driven
	// deadline fresh and detect close.
	conn.SetReadLimit(512)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(wsPongWait)) })
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	s.hub.remove(c)
	close(c.send)
}
