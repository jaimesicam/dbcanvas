// dbcanvas-relay carries one TCP connection over its stdin and stdout. DBCanvas copies it
// into a container on a stack's network that has no bash (a k3s node, a distroless image)
// and runs it there through the Docker API, as the relay of a tunnelled connection
// (app/nodedial.go, app/noderelay.go). It behaves like the bash relay: connect, announce
// it on stderr, copy both ways, and stop when the caller closes its side.
//
// Usage: dbcanvas-relay <host:port>
//
// It also carries a virtual machine's share of a stack network (tun.go): dbcanvas-relay tun …
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// ready must match relayOK in app/nodedial.go.
const ready = "dbcanvas-relay-ok\n"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "tun" {
		os.Exit(tunMain(os.Args[2:]))
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: dbcanvas-relay <host:port>")
		os.Exit(2)
	}
	c, err := net.DialTimeout("tcp", os.Args[1], 10*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(7)
	}
	os.Stderr.WriteString(ready)
	go func() {
		io.Copy(os.Stdout, c)
		os.Exit(0)
	}()
	io.Copy(c, os.Stdin)
	c.Close()
	os.Exit(0)
}
