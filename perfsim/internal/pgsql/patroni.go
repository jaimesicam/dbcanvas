package pgsql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Patroni owns two things the PostgreSQL stressors touch, so on a Patroni
// cluster they go through Patroni's REST API instead of SQL:
//
//   - Replay pause. Patroni's control loop resumes WAL replay on a standby
//     within seconds unless the cluster is in maintenance mode. So the pause
//     puts the cluster in maintenance mode first (which also stops automatic
//     failover while it holds) and takes it out on release, unless it was
//     already in maintenance mode, which is then left as it was.
//   - recovery_min_apply_delay. Patroni passes it to PostgreSQL itself, which
//     overrides ALTER SYSTEM. It is set in Patroni's dynamic configuration
//     instead, which applies it to every standby of the cluster.
//
// The REST API is on every member (8008) and needs no credentials on a
// DBCanvas stack; any member's answers for the whole cluster.
const patroniPort = 8008

func (e *Engine) patroni() bool { return e.cfg.Manager == "patroni" }

func (e *Engine) patroniCall(ctx context.Context, method, path string, body any) ([]byte, error) {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	client := http.Client{Timeout: 5 * time.Second}
	var last error
	for _, m := range e.cfg.Members {
		req, _ := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://%s:%d%s", m.Host, patroniPort, path), bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			last = fmt.Errorf("Patroni %s %s answered %d: %s", method, path, resp.StatusCode, short(string(b), 120))
			continue
		}
		return b, nil
	}
	return nil, last
}

// patroniConfig is the part of Patroni's dynamic configuration the sim reads.
type patroniConfig struct {
	Pause      bool `json:"pause"`
	PostgreSQL struct {
		Parameters map[string]any `json:"parameters"`
	} `json:"postgresql"`
}

func (e *Engine) patroniConfig(ctx context.Context) (patroniConfig, error) {
	var c patroniConfig
	b, err := e.patroniCall(ctx, "GET", "/config", nil)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// setPatroniMaintenance turns maintenance mode on or off.
func (e *Engine) setPatroniMaintenance(ctx context.Context, on bool) error {
	var v any // null removes the key, which is how Patroni leaves maintenance mode
	if on {
		v = true
	}
	_, err := e.patroniCall(ctx, "PATCH", "/config", map[string]any{"pause": v})
	return err
}

// setPatroniParam sets one PostgreSQL parameter in Patroni's dynamic
// configuration; an empty value removes it.
func (e *Engine) setPatroniParam(ctx context.Context, name, value string) error {
	var v any
	if value != "" {
		v = value
	}
	_, err := e.patroniCall(ctx, "PATCH", "/config", map[string]any{"postgresql": map[string]any{"parameters": map[string]any{name: v}}})
	return err
}
