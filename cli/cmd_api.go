package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// cmd_api.go — the escape hatch, and the reason this CLI is complete on day one.
//
// DBCanvas has 200-odd endpoints and gains more most weeks. Hand-writing a
// subcommand for each would be a second, always-lagging copy of the API's surface.
// So `dbcanvas api METHOD PATH` reaches any of them with the token applied and the
// same error reporting as everything else, `dbcanvas endpoints` is how you find the
// one you want, and the curated commands are ergonomics for the twenty verbs people
// actually type daily — not the boundary of the tool.

func cmdAPI(args []string) error {
	fs := flagsFor("api")
	data := fs.String("data", "", "request body: inline JSON, @file, or @- for stdin")
	raw := fs.Bool("raw", false, "print the response exactly as received, without reformatting")
	out := fs.String("out", "", "save the response body to this file (for download endpoints)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if nargs() < 1 {
		fmt.Fprintln(os.Stderr, "dbcanvas: usage: dbcanvas api [METHOD] <path> [--data '{…}']")
		fmt.Fprintln(os.Stderr, "  e.g. dbcanvas api GET /api/stacks")
		fmt.Fprintln(os.Stderr, "       dbcanvas api POST /api/stacks/1/deploy")
		return errUsage
	}

	// Both `api GET /path` and `api /path` are accepted; the method defaults to GET
	// with no body and POST with one, which is what somebody means either way.
	method, path := "", ""
	if nargs() >= 2 {
		method, path = strings.ToUpper(arg(0)), arg(1)
	} else {
		path = arg(0)
		if *data != "" {
			method = "POST"
		} else {
			method = "GET"
		}
	}
	switch method {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD":
	default:
		return fmt.Errorf("%q is not an HTTP method — did you mean `dbcanvas api GET %s`?", method, method)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !strings.HasPrefix(path, "/api/") {
		return fmt.Errorf("%q is not an API path — they all start with /api/ (see `dbcanvas endpoints`)", path)
	}

	c, err := mustClient()
	if err != nil {
		return err
	}

	if *out != "" {
		written, err := c.download(path, *out)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Saved to %s\n", written)
		return nil
	}

	var body any
	if *data != "" {
		b, err := bodyFrom(*data)
		if err != nil {
			return err
		}
		body = b
	}
	var resp []byte
	if err := c.request(method, path, body, &resp); err != nil {
		return err
	}
	if len(resp) == 0 {
		fmt.Fprintf(os.Stderr, "%s %s: ok (no content)\n", method, path)
		return nil
	}
	if *raw {
		_, err := os.Stdout.Write(resp)
		return err
	}
	return printRaw(resp)
}

// bodyFrom resolves --data: inline JSON, @file, or @- for stdin. It validates the
// JSON here rather than letting the server reject it, because a 400 from a typo in a
// quoted shell string is a worse error message than a parse error naming the offset.
func bodyFrom(spec string) ([]byte, error) {
	var raw []byte
	var err error
	switch {
	case spec == "@-":
		raw, err = readAll(os.Stdin)
	case strings.HasPrefix(spec, "@"):
		raw, err = os.ReadFile(spec[1:])
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", spec[1:], err)
		}
	default:
		raw = []byte(spec)
	}
	if err != nil {
		return nil, err
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("the request body is not valid JSON: %w", err)
	}
	return raw, nil
}
