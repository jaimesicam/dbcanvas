// Package valkey is Performance Sim's Valkey engine: a standalone server or a
// Valkey Cluster, whose shards are each a primary with replicas.
package valkey

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"perfsim/internal/core"
)

// Config is what DBCanvas resolved: every member, and the password of the
// "default" user.
type Config struct {
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	Password string         `json:"password"`
	Members  []MemberConfig `json:"members"`
}

type MemberConfig struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
	Role string `json:"role"`
}

const ordersKeys = 100000

type Engine struct {
	cfg    Config
	writer redis.UniversalClient // the application's: a cluster client, or the one server
	mc     map[string]*redis.Client

	mu       sync.Mutex
	version  string
	topology string // standalone | cluster
	writerID string
	lag      map[string]float64
	// hbKeys is one heartbeat key per primary, each hashing to a slot that
	// primary owns, so every shard's replicas have a key of their own to measure.
	hbKeys    map[string]string // primary id -> key
	primaryOf map[string]string // replica id -> its primary's id
	debugOK   map[string]bool   // DEBUG allowed on this member (enable-debug-command)
}

func New(cfg Config) *Engine {
	e := &Engine{cfg: cfg, mc: map[string]*redis.Client{}, lag: map[string]float64{}, hbKeys: map[string]string{}, primaryOf: map[string]string{}, debugOK: map[string]bool{}}
	var addrs []string
	for _, m := range cfg.Members {
		addr := fmt.Sprintf("%s:%d", m.Host, m.Port)
		addrs = append(addrs, addr)
		// READONLY lets a cluster replica answer reads of its own copy of a key
		// rather than redirect them to its primary; outside cluster mode it is
		// refused, which is harmless.
		e.mc[m.ID] = redis.NewClient(&redis.Options{Addr: addr, Password: cfg.Password, PoolSize: 8,
			DialTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second,
			OnConnect: func(ctx context.Context, cn *redis.Conn) error { cn.ReadOnly(ctx); return nil }})
	}
	e.writer = redis.NewUniversalClient(&redis.UniversalOptions{Addrs: addrs, Password: cfg.Password, PoolSize: 80,
		DialTimeout: 3 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second})
	return e
}

func (e *Engine) Close() {
	e.writer.Close()
	for _, c := range e.mc {
		c.Close()
	}
}

func (e *Engine) Init(ctx context.Context) error {
	info, err := e.writer.Info(ctx, "server").Result()
	if err != nil {
		return err
	}
	topo := "standalone"
	if strings.Contains(info, "redis_mode:cluster") || strings.Contains(info, "server_mode:cluster") {
		topo = "cluster"
	} else if len(e.cfg.Members) > 1 {
		topo = "replication"
	}
	e.mu.Lock()
	e.version, e.topology = field(info, "valkey_version", field(info, "redis_version", "")), topo
	e.mu.Unlock()
	for _, m := range e.cfg.Members {
		ok := e.mc[m.ID].Do(ctx, "DEBUG", "HELP").Err() == nil
		e.mu.Lock()
		e.debugOK[m.ID] = ok
		e.mu.Unlock()
	}
	e.Sample(ctx)
	e.replayUndo(ctx)
	return e.Heartbeat(ctx)
}

func (e *Engine) Info() core.TargetInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return core.TargetInfo{Name: e.cfg.Name, Engine: "valkey", Flavor: "valkey", Version: e.version, Topology: e.topology, Writer: e.writerID,
		LagTitle: "Replication lag", LagSub: "how far each replica's copy of its shard's heartbeat key is behind its primary", Status: "Replication info"}
}

// Heartbeat writes the time on every primary's heartbeat key.
func (e *Engine) Heartbeat(ctx context.Context) error {
	e.mu.Lock()
	keys := make([]string, 0, len(e.hbKeys))
	for _, k := range e.hbKeys {
		keys = append(keys, k)
	}
	e.mu.Unlock()
	if len(keys) == 0 {
		keys = []string{"perfsim:hb"}
	}
	now := strconv.FormatInt(time.Now().UnixMicro(), 10)
	var first error
	for _, k := range keys {
		if err := e.writer.Set(ctx, k, now, 0).Err(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (e *Engine) Op(ctx context.Context, write bool, n int) error {
	key := "perfsim:o:" + strconv.Itoa(n%ordersKeys+1)
	if write {
		return e.writer.HSet(ctx, key, "customer", n%10000, "amount", n%50000, "status", "new").Err()
	}
	err := e.writer.HGetAll(ctx, key).Err()
	if err == redis.Nil {
		return nil
	}
	return err
}

// --- saved state, in one hash ---

const stateKey = "perfsim:state"

func (e *Engine) Save(ctx context.Context, key string, v []byte) error {
	return e.writer.HSet(ctx, stateKey, key, string(v)).Err()
}

func (e *Engine) Load(ctx context.Context, prefix string) (map[string][]byte, error) {
	all, err := e.writer.HGetAll(ctx, stateKey).Result()
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for k, v := range all {
		if strings.HasPrefix(k, prefix) {
			out[k] = []byte(v)
		}
	}
	return out, nil
}

func (e *Engine) Delete(ctx context.Context, key string) error {
	return e.writer.HDel(ctx, stateKey, key).Err()
}

// Valkey needs no prepared tables: the stressors that want a big key build it.
func (e *Engine) Datasets(ctx context.Context) ([]core.Dataset, error) { return nil, nil }

func (e *Engine) Prepare(ctx context.Context, name string, bytes int64, pk bool, progress func(float64)) error {
	return fmt.Errorf("Valkey needs no prepared tables")
}

func (e *Engine) Drop(ctx context.Context, name string) error {
	return fmt.Errorf("Valkey has no prepared tables")
}

func (e *Engine) member(id string) *redis.Client { return e.mc[id] }

func (e *Engine) lagOf(id string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v, ok := e.lag[id]; ok {
		return v
	}
	return -1
}

func (e *Engine) replicaIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for id := range e.primaryOf {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// memberByAddr maps "ip:port" or "host:port" from ROLE or CLUSTER output to a member id.
func (e *Engine) memberByAddr(ctx context.Context, addr string) string {
	host, port, _ := strings.Cut(addr, ":")
	for _, m := range e.cfg.Members {
		if (m.Host == host || strings.Split(m.Host, ".")[0] == strings.Split(host, ".")[0]) && strconv.Itoa(m.Port) == port {
			return m.ID
		}
	}
	return ""
}

// field reads "name:value" from INFO output.
func field(info, name, def string) string {
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return def
}
