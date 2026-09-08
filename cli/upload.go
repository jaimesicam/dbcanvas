package main

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

// upload.go — the multipart POSTs.
//
// Everything here streams. A node file drop can be gigabytes (the Core Dump Analyzer
// exists because people move 800 MB cores around), and buffering one in memory to
// build a request body would fail exactly when the feature matters most. So the
// multipart body is written into an io.Pipe and handed to the request as a reader:
// the file goes from disk to socket without ever being fully resident.

// uploadFile streams one file into a node's directory.
func uploadFile(c *Client, stackID int64, nodeID, dir, name string, r io.Reader, size int64) error {
	path := fmt.Sprintf("/api/stacks/%d/nodes/%s/fs/upload", stackID, url.PathEscape(nodeID))
	return streamMultipart(c, path, map[string]string{"path": dir}, "files", name, r)
}

// uploadTo posts a file to an endpoint that takes a single `file` part — the Packet
// Inspector, Log Summary and Stalk Summary uploads.
func uploadTo(c *Client, path, field, localPath string, fields map[string]string) ([]byte, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", localPath, err)
	}
	defer f.Close()
	var out []byte
	if err := streamMultipartOut(c, path, fields, field, filepath.Base(localPath), f, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func streamMultipart(c *Client, path string, fields map[string]string, field, name string, r io.Reader) error {
	return streamMultipartOut(c, path, fields, field, name, r, nil)
}

func streamMultipartOut(c *Client, path string, fields map[string]string, field, name string, r io.Reader, out *[]byte) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		// Any error here has to reach the reader, or the request would hang or send
		// a silently truncated body. CloseWithError is what surfaces it.
		var err error
		defer func() { pw.CloseWithError(err) }()
		for k, v := range fields {
			if err = mw.WriteField(k, v); err != nil {
				return
			}
		}
		var part io.Writer
		if part, err = mw.CreateFormFile(field, name); err != nil {
			return
		}
		if _, err = io.Copy(part, r); err != nil {
			return
		}
		err = mw.Close()
	}()

	req, err := http.NewRequest("POST", c.BaseURL+path, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	// No client timeout for an upload: the 120s default would cap a large file at
	// whatever fits in two minutes, which is not a limit anybody asked for.
	uploader := &http.Client{Transport: c.HTTP.Transport}
	resp, err := uploader.Do(req)
	if err != nil {
		return fmt.Errorf("upload to %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Message: apiMessage(raw, resp.StatusCode), Method: "POST", Path: path}
	}
	if out != nil {
		*out = raw
	}
	return nil
}
