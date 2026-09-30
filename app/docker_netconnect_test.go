package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
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
