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

	// The same through openStackDB, which the Query Runner and Benchmark use.
	if a.store, err = OpenStore(t.TempDir() + "/it.db"); err != nil {
		t.Fatal(err)
	}
	db2, err := a.openStackDB(0, cid, "mysql", qrMySQLDSN(cfg.User, cfg.Passwd, addr, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := db2.QueryRowContext(ctx, "SELECT @@hostname").Scan(&host); err != nil {
		t.Fatalf("openStackDB mysql: %v", err)
	}

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
	} else {
		t.Logf("closed port: %v", err)
	}

	// An address that drops packets fails at the caller's deadline instead of hanging.
	short, cancelShort := context.WithTimeout(ctx, 2*time.Second)
	defer cancelShort()
	start = time.Now()
	if _, err := a.tunnelDial(short, 0, cid, "192.0.2.1:3306"); err == nil {
		t.Fatal("an unreachable address was accepted")
	} else if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("unreachable address took %s: %v", took, err)
	} else {
		t.Logf("unreachable address after %s: %v", took.Round(time.Millisecond), err)
	}
}

// TestTunnelReachesPostgres opens a pgx pool through openStackDB, tunnelled:
//
//	DOCKER_IT=1 NODEDIAL_IT_SOCK=<docker socket> NODEDIAL_IT_PG_CONTAINER=<container>
//	NODEDIAL_IT_PG_ADDR=<host:port on its network> NODEDIAL_IT_PG_PASS=<postgres password>
func TestTunnelReachesPostgres(t *testing.T) {
	if os.Getenv("DOCKER_IT") == "" || os.Getenv("NODEDIAL_IT_PG_CONTAINER") == "" {
		t.Skip("integration test; see the comment for the environment it needs")
	}
	t.Setenv("DBCANVAS_NODE_DIAL", "tunnel")
	sock := firstNonEmpty(os.Getenv("NODEDIAL_IT_SOCK"), "/var/run/docker.sock")
	a := &App{docker: NewDocker(sock)}
	var err error
	if a.store, err = OpenStore(t.TempDir() + "/it.db"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := "postgres://postgres:" + os.Getenv("NODEDIAL_IT_PG_PASS") + "@" + os.Getenv("NODEDIAL_IT_PG_ADDR") + "/postgres?sslmode=prefer&connect_timeout=10"
	db, err := a.openStackDB(0, os.Getenv("NODEDIAL_IT_PG_CONTAINER"), "pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	var n int
	if err := db.QueryRowContext(ctx, "SELECT version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT repeat('x', 1000) FROM generate_series(1, 20000)) t").Scan(&n); err != nil || n != 20000 {
		t.Fatalf("20000 rows: %d, %v", n, err)
	}
	t.Logf("through the tunnel: %s", version)
}

// TestTunnelReachesMongoAndValkey opens a MongoDB client (mongoClientFor) and a Valkey
// connection (the DB Explorer's adapter) through the tunnel:
//
//	DOCKER_IT=1 NODEDIAL_IT_SOCK=<docker socket>
//	NODEDIAL_IT_MONGO_CONTAINER=<container> NODEDIAL_IT_MONGO_HOST=<its name on the network> NODEDIAL_IT_MONGO_PASS=<root password>
//	NODEDIAL_IT_VALKEY_CONTAINER=<container> NODEDIAL_IT_VALKEY_ADDR=<host:port on the network>
func TestTunnelReachesMongoAndValkey(t *testing.T) {
	if os.Getenv("DOCKER_IT") == "" || os.Getenv("NODEDIAL_IT_MONGO_CONTAINER") == "" {
		t.Skip("integration test; see the comment for the environment it needs")
	}
	t.Setenv("DBCANVAS_NODE_DIAL", "tunnel")
	sock := firstNonEmpty(os.Getenv("NODEDIAL_IT_SOCK"), "/var/run/docker.sock")
	a := &App{docker: NewDocker(sock)}
	var err error
	if a.store, err = OpenStore(t.TempDir() + "/it.db"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	c := dbConn{ContainerID: os.Getenv("NODEDIAL_IT_MONGO_CONTAINER"), Addr: os.Getenv("NODEDIAL_IT_MONGO_HOST"),
		Super: "root", Password: os.Getenv("NODEDIAL_IT_MONGO_PASS"), eng: a.docker}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer()
	var info struct {
		Version string `bson:"version"`
	}
	if err := client.Database("admin").RunCommand(ctx, map[string]any{"buildInfo": 1}).Decode(&info); err != nil {
		t.Fatal(err)
	}
	t.Logf("MongoDB %s through the tunnel", info.Version)

	v := &dexValkeyAdapter{addr: os.Getenv("NODEDIAL_IT_VALKEY_ADDR"),
		dial: a.stackNodeDial(0, os.Getenv("NODEDIAL_IT_VALKEY_CONTAINER"))}
	if err := v.TestConnection(ctx); err != nil {
		t.Fatalf("valkey: %v", err)
	}
	t.Log("Valkey answered through the tunnel")
}
