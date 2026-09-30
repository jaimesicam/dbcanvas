package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// A watcher's VNC stream: the handshake and screen requests reach the desktop; keys,
// the pointer, the clipboard and resizes do not — whatever pieces they arrive in.
func TestVNCViewOnlyFilter(t *testing.T) {
	var in, want bytes.Buffer
	emit := func(b []byte, pass bool) {
		in.Write(b)
		if pass {
			want.Write(b)
		}
	}
	emit([]byte("RFB 003.008\n"), true)
	emit([]byte{2}, true)                              // VNC authentication
	emit(bytes.Repeat([]byte{7}, 16), true)            // the response
	in.WriteByte(0)                                    // ClientInit asking for exclusive…
	want.WriteByte(1)                                  // …is sent as shared
	emit(append([]byte{0}, make([]byte, 19)...), true) // SetPixelFormat
	enc := []byte{2, 0, 0, 2, 0, 0, 0, 7, 0xff, 0xff, 0xff, 0x21}
	emit(enc, true)                                  // SetEncodings, two
	emit([]byte{3, 1, 0, 0, 0, 0, 4, 0, 3, 0}, true) // FramebufferUpdateRequest
	emit([]byte{4, 1, 0, 0, 0, 0, 0, 0x61}, false)   // KeyEvent 'a'
	emit([]byte{5, 1, 0, 10, 0, 20}, false)          // PointerEvent click
	cut := make([]byte, 8)
	cut[0] = 6
	binary.BigEndian.PutUint32(cut[4:], 5)
	emit(append(cut, []byte("hello")...), false)                                                        // ClientCutText
	emit([]byte{251, 0, 0x04, 0, 0x03, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, false) // SetDesktopSize, 1 screen
	emit([]byte{3, 1, 0, 0, 0, 0, 4, 0, 3, 0}, true)

	// Byte by byte: pieces never line up with messages on a real socket.
	f := &rfbViewFilter{}
	var got bytes.Buffer
	for _, b := range in.Bytes() {
		out, err := f.feed([]byte{b})
		if err != nil {
			t.Fatalf("filter error at byte %d: %v", got.Len(), err)
		}
		got.Write(out)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Errorf("forwarded %d bytes, want %d\n got % x\nwant % x", got.Len(), want.Len(), got.Bytes(), want.Bytes())
	}

	// Something it cannot read closes the connection rather than guessing.
	f = &rfbViewFilter{}
	f.feed([]byte("RFB 003.008\n\x01\x00"))
	if _, err := f.feed([]byte{99, 1, 2, 3}); err == nil {
		t.Error("an unknown client message was let through")
	}
	if _, err := (&rfbViewFilter{}).feed([]byte("RFB 003.003\n")); err == nil {
		t.Error("RFB 3.3, whose security the filter cannot follow, was accepted")
	}
}
