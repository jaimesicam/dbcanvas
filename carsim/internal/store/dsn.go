package store

import (
	"net/url"
	"strings"
)

// withDatabase returns dsn with its path (the database name) replaced by
// dbName, everything else (host, auth, query params) preserved.
func withDatabase(dsn, dbName string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

// quoteIdent double-quotes a Postgres identifier for use in DDL where a bind
// parameter isn't allowed (e.g. CREATE DATABASE). dbName always originates from
// this app's own fixed constants ("carsim"), never external input, but this is
// cheap insurance against a literal embedded quote either way.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
