package store

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// TestIsHostgroupLocked guards the classifier the Objects() recovery turns on.
//
// The failure it exists for is quiet: a read proxy pins the session to the writer
// when it sees a SET it does not track, and then refuses to route the SELECT that
// follows. If this predicate stops matching, Objects() keeps returning the error
// AND keeps handing the pinned connection back to the pool, where it breaks the
// next reader to be given it — which is how a bug in information_schema reads
// showed up in the event feed.
//
// The CONFIRMED case below is the exact text ProxySQL 2.7 returned in front of a
// three-node PXC cluster, copied from a live session rather than paraphrased.
func TestIsHostgroupLocked(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"the real ProxySQL error", &mysqldriver.MySQLError{
			Number:  9006,
			Message: "ProxySQL Error: connection is locked to hostgroup 10 but trying to reach hostgroup 11",
		}, true},
		{"wrapped, as the callers see it", fmt.Errorf("measure: %w", &mysqldriver.MySQLError{
			Number:  9006,
			Message: "ProxySQL Error: connection is locked to hostgroup 10 but trying to reach hostgroup 11",
		}), true},
		// 9006 is ProxySQL's general-purpose number, so the code alone is not
		// enough — retrying without fresh statistics fixes none of these.
		{"a different 9006", &mysqldriver.MySQLError{
			Number: 9006, Message: "ProxySQL Error: Max connect timeout reached",
		}, false},
		{"an ordinary MySQL error", &mysqldriver.MySQLError{
			Number: 1146, Message: "Table 'stocksim.events' doesn't exist",
		}, false},
		{"a plain error carrying the words", errors.New("locked to hostgroup 10"), false},
		{"nil", nil, false},
	} {
		if got := isHostgroupLocked(c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestNoSessionStateOnThePool: a session-scoped SET on *sql.DB reaches an arbitrary
// pooled connection, so it neither reliably applies to the statements that follow nor
// goes away afterwards — and behind a read proxy it pins that connection to the writer
// for good. Objects() takes its own *sql.Conn; nothing here may SET on the pool. Wipe
// and DropSchema used to, disabling foreign-key checks for a schema that declares no
// foreign keys.
func TestNoSessionStateOnThePool(t *testing.T) {
	src, err := os.ReadFile("mysql.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), `s.db.ExecContext(ctx, "SET `); n != 0 {
		t.Errorf("%d session SET(s) on the pool — put them on a *sql.Conn, or drop them", n)
	}
}
