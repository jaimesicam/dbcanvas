package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// client.go — the HTTP layer, and the error messages that come out of it.
//
// The errors are the interesting part. A CLI's failure output is its documentation
// for everything that goes wrong, so a 401 says "run dbcanvas login" and a 403 says
// which scope was needed, rather than printing a status code and leaving the reader
// to guess which of the two problems they have. The exit codes divide the same way
// (see main.go), because a CI job needs to tell "my token lapsed" from "the cluster
// failed to come up".

// Client talks to one installation.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// jar is set only for the brief password-authenticated session `login` uses.
	// Every other command is stateless and bearer-authenticated.
	jar *cookiejar.Jar
}

func newClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		// Generous, because deploy, validate and the analysis endpoints do real
		// work before answering. Long-running operations are polled, not held open,
		// so this is a ceiling on one request rather than on one operation.
		HTTP: &http.Client{Timeout: 120 * time.Second},
	}
}

// withCookies gives the client a cookie jar, for the login handshake only.
func (c *Client) withCookies() error {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	c.jar = jar
	c.HTTP.Jar = jar
	return nil
}

// APIError is a non-2xx response, carrying enough to choose an exit code.
type APIError struct {
	Status  int
	Message string
	Method  string
	Path    string
}

func (e *APIError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "not signed in, or the token has expired or been revoked — run `dbcanvas login`"
	case http.StatusForbidden:
		// The server's message names the scope or the ownership problem, and is
		// more specific than anything that could be said here.
		return e.Message
	case http.StatusNotFound:
		return fmt.Sprintf("%s: %s", e.Message, e.Path)
	default:
		return e.Message
	}
}

// exitCode maps a failure onto the documented codes.
func exitCodeFor(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		return 3
	}
	if errors.Is(err, errNotConfigured) {
		return 3
	}
	if errors.Is(err, errWaitFailed) {
		return 4
	}
	if errors.Is(err, errUsage) {
		return 2
	}
	return 1
}

var (
	errWaitFailed = errors.New("the operation did not finish successfully")
	errUsage      = errors.New("usage")
)

// request performs one call. body may be nil, a []byte of pre-encoded JSON, or any
// value to marshal. out may be nil, a *[]byte for raw bytes, or a pointer to decode
// JSON into.
func (c *Client) request(method, path string, body, out any) error {
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rdr = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// A connection error is almost always the URL, so name it.
		return fmt.Errorf("cannot reach %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("request failed (%d)", resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Message: msg, Method: method, Path: path}
	}

	switch o := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*o = raw
		return nil
	default:
		if len(raw) == 0 {
			return nil
		}
		return json.Unmarshal(raw, out)
	}
}

func (c *Client) get(path string, out any) error { return c.request("GET", path, nil, out) }
func (c *Client) post(path string, body, out any) error {
	return c.request("POST", path, body, out)
}
func (c *Client) delete(path string, out any) error { return c.request("DELETE", path, nil, out) }

// download streams a file endpoint to disk, honouring the server's
// Content-Disposition filename. Returns the path written.
func (c *Client) download(path, into string) (string, error) {
	req, err := http.NewRequest("GET", c.BaseURL+path, nil)
	if err != nil {
		return "", err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(resp.Body)
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return "", &APIError{Status: resp.StatusCode, Message: msg, Method: "GET", Path: path}
	}

	name := filepath.Base(path)
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil && params["filename"] != "" {
			// Base() defends against a server (or a proxy) offering a filename with
			// a path in it.
			name = filepath.Base(params["filename"])
		}
	}
	dest := name
	if into != "" {
		if st, err := os.Stat(into); err == nil && st.IsDir() {
			dest = filepath.Join(into, name)
		} else {
			dest = into
		}
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", err
	}
	return dest, nil
}

// wsURL turns an API path into the WebSocket URL for it, preserving the scheme.
func (c *Client) wsURL(path string) (string, error) {
	u, err := url.Parse(c.BaseURL + path)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	return u.String(), nil
}
