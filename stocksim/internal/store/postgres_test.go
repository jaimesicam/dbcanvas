package store

import "testing"

// pgWithDatabase and pgDatabaseOf are what decide, and then report, which
// database this app's objects end up in — the difference between "a database of
// its own" and "a schema inside someone else's". Both DSN forms have to work:
// linked nodes get a URL, and a user pasting a connection string under Advanced
// may well paste keyword/value.

func TestPgWithDatabase(t *testing.T) {
	cases := []struct{ dsn, want string }{
		{
			"postgres://u:p@db:5432/postgres?sslmode=prefer",
			"postgres://u:p@db:5432/stocksim?sslmode=prefer",
		},
		{
			"postgres://u:p@db:5432/?sslmode=require&connect_timeout=10",
			"postgres://u:p@db:5432/stocksim?sslmode=require&connect_timeout=10",
		},
		{
			"host=db port=5432 user=u dbname=postgres",
			"host=db port=5432 user=u dbname=postgres dbname='stocksim'",
		},
	}
	for _, c := range cases {
		if got := pgWithDatabase(c.dsn, "stocksim"); got != c.want {
			t.Errorf("pgWithDatabase(%q) = %q, want %q", c.dsn, got, c.want)
		}
	}
}

func TestPgDatabaseOf(t *testing.T) {
	for dsn, want := range map[string]string{
		"postgres://u:p@db:5432/postgres?sslmode=prefer": "postgres",
		"postgres://u:p@db:5432/stocksim":                "stocksim",
		"postgres://u:p@db:5432/?sslmode=require":        "",
		"host=db user=u dbname=appdb":                    "appdb",
		"host=db user=u dbname='app db'":                 "app",
		"host=db user=u":                                 "",
	} {
		if got := pgDatabaseOf(dsn); got != want {
			t.Errorf("pgDatabaseOf(%q) = %q, want %q", dsn, got, want)
		}
	}
}

// Location is the answer to "where did my data actually land", so both
// placements have to describe themselves unambiguously.
func TestPgStoreLocation(t *testing.T) {
	owned := &pgStore{name: "stocksim", database: "stocksim", schema: "public", owned: true}
	if got, want := owned.Location(), `database "stocksim" (schema public)`; got != want {
		t.Errorf("owned Location() = %q, want %q", got, want)
	}
	if got, want := owned.Database(), "stocksim"; got != want {
		t.Errorf("owned Database() = %q, want %q", got, want)
	}

	fallback := &pgStore{name: "stocksim", database: "postgres", schema: "stocksim"}
	if got, want := fallback.Location(), `schema "stocksim" inside database "postgres"`; got != want {
		t.Errorf("fallback Location() = %q, want %q", got, want)
	}
	// Database() reports the namespace the user asked for either way, because
	// that is what the drop confirmation asks them to type back.
	if got, want := fallback.Database(), "stocksim"; got != want {
		t.Errorf("fallback Database() = %q, want %q", got, want)
	}
}
