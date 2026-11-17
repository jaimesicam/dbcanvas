package main

import (
	"context"
	"database/sql"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// TestTunnelReachesAStackNode runs a MySQL session through the Docker API tunnel, the way
// DBCanvas reaches nodes where their addresses are not routable (macOS). It needs a
// running MySQL-family node:
//
//	DOCKER_IT=1 NODEDIAL_IT_SOCK=<docker socket> NODEDIAL_IT_CONTAINER=<container>
//	NODEDIAL_IT_ADDR=<host:port as seen on the stack network> NODEDIAL_IT_USER=<account allowed from the network> NODEDIAL_IT_PASS=<its password>
func TestTunnelReachesAStackNode(t *testing.T) {
	if os.Getenv("DOCKER_IT") == "" || os.Getenv("NODEDIAL_IT_CONTAINER") == "" {
		t.Skip("integration test; see the comment for the environment it needs")
	}
	sock := firstNonEmpty(os.Getenv("NODEDIAL_IT_SOCK"), "/var/run/docker.sock")
	a := &App{docker: NewDocker(sock)}
	cid, addr := os.Getenv("NODEDIAL_IT_CONTAINER"), os.Getenv("NODEDIAL_IT_ADDR")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// The raw stream: MySQL's server speaks first, a protocol-10 greeting.
	c, err := a.tunnelDial(ctx, 0, cid, addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(c, hdr); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if hdr[4] != 10 {
		t.Fatalf("greeting protocol %d, want 10", hdr[4])
	}
	c.Close()

	// A whole session through the driver's dial hook.
	t.Setenv("DBCANVAS_NODE_DIAL", "tunnel")
	mysql.RegisterDialContext("nodedial-it", func(ctx context.Context, addr string) (net.Conn, error) {
		return a.stackDial(0, cid)(ctx, "tcp", addr)
	})
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = firstNonEmpty(os.Getenv("NODEDIAL_IT_USER"), "root"), os.Getenv("NODEDIAL_IT_PASS"), "nodedial-it", addr
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var host, version string
	if err := db.QueryRowContext(ctx, "SELECT @@hostname, @@version").Scan(&host, &version); err != nil {
		t.Fatal(err)
	}
	// Enough rows to cross several Docker frames.
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT REPEAT('x', 1000) FROM information_schema.columns a, information_schema.columns b LIMIT 20000) t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	var big string
	if err := db.QueryRowContext(ctx, "SELECT REPEAT('abc', 200000)").Scan(&big); err != nil || len(big) != 600000 || strings.Trim(big, "abc") != "" {
		t.Fatalf("600 kB value through the tunnel: len %d, err %v", len(big), err)
	}
	t.Logf("through the tunnel: %s, MySQL %s, %d rows counted, 600 kB value intact", host, version, n)

	// What opening a connection costs: dial plus the server's greeting.
	start := time.Now()
	for i := 0; i < 10; i++ {
		c, err := a.tunnelDial(ctx, 0, cid, addr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	t.Logf("open a connection and read the greeting: %s on average", (time.Since(start) / 10).Round(time.Millisecond))

	// A refused port is an error at dial time, not a connection that hangs.
	if _, err := a.tunnelDial(ctx, 0, cid, strings.Split(addr, ":")[0]+":1"); err == nil {
		t.Fatal("a closed port was accepted")
	}
}
