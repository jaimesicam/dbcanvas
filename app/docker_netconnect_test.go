package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// The app joining a stack network must never take that network as its default
// gateway: Docker publishes ports on the gateway network, so moving it restarts the
// proxy behind :8080 and cuts every terminal and stream open through it.
func TestNetworkConnectNeverTakesTheGateway(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	var got map[string]any
	var path string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(200)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	if err := NewDocker(sock).NetworkConnect(context.Background(), "dbcanvas-stack-7", "app"); err != nil {
		t.Fatal(err)
	}
	if path != "/networks/dbcanvas-stack-7/connect" {
		t.Fatalf("path = %q", path)
	}
	ep, _ := got["EndpointConfig"].(map[string]any)
	if got["Container"] != "app" || ep == nil || ep["GwPriority"] != float64(-1) {
		t.Fatalf("body = %v, want Container=app and EndpointConfig.GwPriority=-1", got)
	}
}

// A refused network create says why: Docker's message, not only its status. The body used to be
// drained before the error was built from it, leaving "docker create network: (400)".
func TestNetworkCreateErrorCarriesDockersMessage(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(400)
		w.Write([]byte(`{"message":"all predefined address pools have been fully subnetted"}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	err = NewDocker(sock).NetworkEnsure(context.Background(), "dbcanvas-stack-7")
	if err == nil || !strings.Contains(err.Error(), "fully subnetted") {
		t.Fatalf("err = %v, want Docker's message in it", err)
	}
}
