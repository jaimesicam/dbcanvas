package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/coder/websocket"
)

// handleNodeTerminal upgrades to a WebSocket and bridges it to an interactive
// `bash` exec (TTY) in the node's container. Browser → container: binary frames
// are raw keystrokes; text frames are control messages (`{"type":"resize",...}`).
// Container → browser: raw pty output as binary frames.
func (a *App) handleNodeTerminal(w http.ResponseWriter, r *http.Request) {
	// Auth + resolve a running node (writes an HTTP error pre-upgrade on failure).
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin in production; relaxed so the Vite dev proxy works too.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := a.docker.HijackExec(ctx, dep.ContainerID,
		[]string{"/bin/bash"}, []string{"TERM=xterm-256color"})
	if err != nil {
		c.Close(websocket.StatusInternalError, "exec failed")
		return
	}
	defer stream.Close()

	// container → browser
	go func() {
		defer cancel()
		buf := make([]byte, 4096)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if werr := c.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// browser → container
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageText {
			var msg struct {
				Type       string `json:"type"`
				Cols, Rows int
			}
			if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
				a.docker.ResizeExec(ctx, stream.ExecID, msg.Cols, msg.Rows)
				continue
			}
		}
		if _, err := stream.Write(data); err != nil {
			break
		}
	}
}
