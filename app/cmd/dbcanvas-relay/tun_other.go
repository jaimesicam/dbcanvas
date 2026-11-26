//go:build !linux

package main

import (
	"fmt"
	"os"
)

// tunMain needs Linux's TUN devices; the relay only ever runs on Linux nodes.
func tunMain(args []string) int {
	fmt.Fprintln(os.Stderr, "tun: Linux only")
	return 2
}
