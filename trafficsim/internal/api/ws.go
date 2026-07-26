package api

import (
	"net/http"
	"time"

	"github.com/coder/websocket"
	"trafficsim/internal/store"
)

// handleWS bridges Valkey Pub/Sub (ts:notify) to a browser WebSocket connection. This
// is a convenience push channel only — a client that misses messages (a dropped
// connection, a page it hadn't opened yet) always recovers via GET /api/state, never
// by relying on every individual push having arrived (spec §15).
func (h *Handler) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx := conn.CloseRead(r.Context()) // detects client-initiated close, discards any client->server frames

	sub := h.Store.Client.Subscribe(ctx, store.NotifyChan)
	defer sub.Close()
	ch := sub.Channel()

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
			if err := conn.Write(ctx, websocket.MessageText, []byte(msg.Payload)); err != nil {
				return
			}
		}
	}
}
