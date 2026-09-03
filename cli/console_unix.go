//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// console_unix.go — noticing a resized terminal, the way Unix offers.
//
// SIGWINCH is delivered the moment the window changes, so the remote shell reflows
// immediately and for free. Windows has no such signal; console_windows.go polls
// instead. Splitting the two is not gold-plating — the CLI is cross-compiled for
// five platforms in the app image, and `syscall.SIGWINCH` does not exist on Windows,
// so a single file would simply fail to build.

// watchResize calls onResize whenever the terminal is resized, and returns a
// function that stops watching.
func watchResize(fd int, onResize func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				onResize()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
