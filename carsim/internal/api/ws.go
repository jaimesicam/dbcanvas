package api

import (
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// handleWS bridges the engine's in-process EventBus to a browser WebSocket
// connection. This is a convenience push channel only — a client that misses
// messages (a dropped connection, a page it hadn't opened yet) always recovers via
// GET /api/state and GET /api/events, never by relying on every individual push
// having arrived.
func (h *Handler) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx := conn.CloseRead(r.Context()) // detects client-initiated close, discards any client->server frames

	id, ch := h.Engine.Bus.Subscribe()
	defer h.Engine.Bus.Unsubscribe(id)

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if err := conn.Ping(ctx); err != nil {
				return
			}
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
				return
			}
		}
	}
}
