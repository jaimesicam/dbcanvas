package main

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// browsevnc.go — a VNC desktop a watching guest can see and not touch.
//
// In a shared session a watcher gets read access: they can look, and the driver acts.
// For a web page that is simple — GETs yes, posts and sockets no. A VNC desktop is
// nothing but a socket, so refusing it would leave a watcher with a blank window while
// everyone else watches the driver work. Instead a watcher's noVNC socket runs
// through a filter that reads the RFB protocol the viewer speaks and forwards only
// what a viewer needs — the handshake, its pixel format and encodings, requests for
// screen updates — and drops keys, the pointer, the clipboard and resize requests.
// It also forces the connection to be shared, so a watcher joining never
// disconnects the driver's own session.
//
// The guarantee is on the server, not in noVNC's view_only switch: a watcher who
// edits the page's settings still cannot type into the desktop.

var errRFBUnsupported = errors.New("this VNC protocol variant is not supported for view-only access")

// rfbViewFilter follows the client side of an RFB session and forwards what a viewer
// may send. Bytes arrive in arbitrary pieces; a message is forwarded once whole.
type rfbViewFilter struct {
	buf   []byte
	stage int // 0 ProtocolVersion, 1 security type, 2 VNC auth response, 3 ClientInit, 4 messages
}

// feed takes client bytes and returns what to pass to the server.
func (f *rfbViewFilter) feed(in []byte) ([]byte, error) {
	f.buf = append(f.buf, in...)
	var out []byte
	for {
		switch f.stage {
		case 0: // "RFB 003.008\n"
			if len(f.buf) < 12 {
				return out, nil
			}
			v := string(f.buf[:12])
			if !strings.HasPrefix(v, "RFB 003.") || strings.HasPrefix(v, "RFB 003.003") {
				return out, errRFBUnsupported // 3.3 lets the server pick security, which this filter does not see
			}
			out = append(out, f.buf[:12]...)
			f.buf = f.buf[12:]
			f.stage = 1
		case 1: // the chosen security type
			if len(f.buf) < 1 {
				return out, nil
			}
			switch f.buf[0] {
			case 1: // None
				f.stage = 3
			case 2: // VNC authentication: a 16-byte response follows
				f.stage = 2
			default:
				return out, errRFBUnsupported
			}
			out = append(out, f.buf[0])
			f.buf = f.buf[1:]
		case 2:
			if len(f.buf) < 16 {
				return out, nil
			}
			out = append(out, f.buf[:16]...)
			f.buf = f.buf[16:]
			f.stage = 3
		case 3: // ClientInit: the shared flag, always set for a watcher
			if len(f.buf) < 1 {
				return out, nil
			}
			out = append(out, 1)
			f.buf = f.buf[1:]
			f.stage = 4
		default:
			if len(f.buf) < 1 {
				return out, nil
			}
			n, pass, err := rfbClientMessage(f.buf)
			if err != nil {
				return out, err
			}
			if n == 0 {
				return out, nil // incomplete; wait for more
			}
			if pass {
				out = append(out, f.buf[:n]...)
			}
			f.buf = f.buf[n:]
		}
	}
}

// rfbClientMessage sizes the client message at the head of b, and says whether a
// viewer may send it. 0 means "not all here yet".
func rfbClientMessage(b []byte) (n int, pass bool, err error) {
	need := func(k int) int {
		if len(b) < k {
			return 0
		}
		return k
	}
	switch b[0] {
	case 0: // SetPixelFormat
		return need(20), true, nil
	case 2: // SetEncodings: u16 count at 2, then 4 bytes each
		if len(b) < 4 {
			return 0, false, nil
		}
		return need(4 + 4*int(binary.BigEndian.Uint16(b[2:4]))), true, nil
	case 3: // FramebufferUpdateRequest
		return need(10), true, nil
	case 150: // EnableContinuousUpdates
		return need(10), true, nil
	case 248: // ClientFence: flags(4) at 4, length(1) at 8, then payload
		if len(b) < 9 {
			return 0, false, nil
		}
		return need(9 + int(b[8])), true, nil
	case 4: // KeyEvent
		return need(8), false, nil
	case 5: // PointerEvent
		return need(6), false, nil
	case 6: // ClientCutText: s32 length at 4 (negative = extended clipboard)
		if len(b) < 8 {
			return 0, false, nil
		}
		l := int32(binary.BigEndian.Uint32(b[4:8]))
		if l < 0 {
			l = -l
		}
		if l > 16<<20 {
			return 0, false, errRFBUnsupported
		}
		return need(8 + int(l)), false, nil
	case 251: // SetDesktopSize: u8 screens at 6, 16 bytes each
		if len(b) < 8 {
			return 0, false, nil
		}
		return need(8 + 16*int(b[6])), false, nil
	case 255: // QEMU extended key event
		return need(12), false, nil
	}
	return 0, false, errRFBUnsupported
}

// serveVNCViewOnly relays a watcher's noVNC socket to the node through the filter.
func (a *App) serveVNCViewOnly(t *browseTarget, sub string, w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheme := "ws"
	if t.Scheme == "https" {
		scheme = "wss"
	}
	var protos []string
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			protos = append(protos, p)
		}
	}
	up, resp, err := websocket.Dial(ctx, scheme+"://node"+sub+queryOf(r), &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: t.proxy.Transport}, Subprotocols: protos,
	})
	if err != nil {
		browsePage(w, http.StatusBadGateway, "The desktop did not answer", "DBCanvas could not reach this VNC desktop.")
		return
	}
	defer up.CloseNow()
	up.SetReadLimit(64 << 20)
	chosen := ""
	if resp != nil {
		chosen = resp.Header.Get("Sec-WebSocket-Protocol")
	}
	opts := &websocket.AcceptOptions{InsecureSkipVerify: true}
	if chosen != "" {
		opts.Subprotocols = []string{chosen}
	}
	down, err := websocket.Accept(w, r, opts)
	if err != nil {
		return
	}
	defer down.CloseNow()
	down.SetReadLimit(1 << 20)

	// desktop → watcher, untouched
	go func() {
		defer cancel()
		for {
			typ, b, err := up.Read(ctx)
			if err != nil {
				return
			}
			if down.Write(ctx, typ, b) != nil {
				return
			}
		}
	}()
	// watcher → desktop, through the filter
	f := &rfbViewFilter{}
	for {
		typ, b, err := down.Read(ctx)
		if err != nil {
			return
		}
		out, ferr := f.feed(b)
		if len(out) > 0 {
			if up.Write(ctx, typ, out) != nil {
				return
			}
		}
		if ferr != nil {
			down.Close(websocket.StatusPolicyViolation, "view only")
			return
		}
	}
}

func queryOf(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}
