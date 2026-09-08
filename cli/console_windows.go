//go:build windows

package main

import (
	"time"

	"golang.org/x/term"
)

// console_windows.go — the same job as console_unix.go, without a signal to do it.
//
// Windows has no SIGWINCH, so the size is polled. Once a second is far more often
// than anybody resizes a window and far too rarely to cost anything, and a
// GetSize call on an already-open handle is cheap. The remote shell reflows within
// a second of the drag finishing, which in practice is indistinguishable from
// immediate.

func watchResize(fd int, onResize func()) func() {
	lastW, lastH, _ := term.GetSize(fd)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				w, h, err := term.GetSize(fd)
				if err != nil || (w == lastW && h == lastH) {
					continue
				}
				lastW, lastH = w, h
				onResize()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}
