package valkey

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"perfsim/internal/core"
)

type memberInfo struct {
	id      string
	up      bool
	primary bool
	master  string // the primary's address, for a replica
	fields  [][2]string
	link    string
	conns   float64
	blocked float64
}

func (e *Engine) Sample(ctx context.Context) ([]core.Member, core.ServerSample) {
	infos := make([]memberInfo, len(e.cfg.Members))
	var wg sync.WaitGroup
	for i, mc := range e.cfg.Members {
		wg.Add(1)
		go func(i int, mc MemberConfig) {
			defer wg.Done()
			infos[i] = e.readInfo(ctx, mc)
		}(i, mc)
	}
	wg.Wait()

	// Which primary each replica follows: INFO gives its address, often an IP.
	ips := e.memberIPs(ctx)
	primaryOf := map[string]string{}
	var primaries []string
	for _, in := range infos {
		if in.primary {
			primaries = append(primaries, in.id)
		} else if in.master != "" {
			host, port, _ := strings.Cut(in.master, ":")
			for id, ip := range ips {
				m := e.memberCfg(id)
				if (ip == host || m.Host == host || strings.Split(m.Host, ".")[0] == host) && strconv.Itoa(m.Port) == port {
					primaryOf[in.id] = id
				}
			}
		}
	}
	e.ensureHeartbeatKeys(ctx, primaries)

	e.mu.Lock()
	e.primaryOf = primaryOf
	if len(primaries) > 0 {
		e.writerID = primaries[0]
	}
	keys := map[string]string{}
	for k, v := range e.hbKeys {
		keys[k] = v
	}
	e.mu.Unlock()

	out := make([]core.Member, len(infos))
	for i, in := range infos {
		m := core.Member{ID: in.id, Lag: -1, Up: in.up, Fields: in.fields, Source: primaryOf[in.id]}
		switch {
		case !in.up:
			m.Role, m.Chip, m.ChipText = "unknown", "bad", "unreachable"
		case in.primary:
			m.Role, m.Chip, m.ChipText, m.Lag = "primary", "ok", "primary", 0
		default:
			m.Role = "replica"
			if key := keys[primaryOf[in.id]]; key != "" {
				if v, err := e.mc[in.id].Get(ctx, key).Result(); err == nil {
					if us, err := strconv.ParseInt(v, 10, 64); err == nil {
						m.Lag = time.Since(time.UnixMicro(us)).Seconds()
						if m.Lag < 0 {
							m.Lag = 0
						}
					}
				}
			}
			switch {
			case in.link != "up":
				m.Chip, m.ChipText = "bad", "link "+in.link
			case m.Lag > 30:
				m.Chip, m.ChipText = "bad", "lagging"
			case m.Lag > 5:
				m.Chip, m.ChipText = "warn", "behind"
			default:
				m.Chip, m.ChipText = "ok", "in sync"
			}
		}
		out[i] = m
	}

	e.mu.Lock()
	for _, m := range out {
		if m.Lag >= 0 {
			e.lag[m.ID] = m.Lag
		} else {
			delete(e.lag, m.ID)
		}
	}
	e.mu.Unlock()

	var srv core.ServerSample
	for _, in := range infos {
		if in.primary {
			srv.Conns += in.conns
			srv.LockWaits += in.blocked
			srv.Threads++
		}
	}
	return out, srv
}

func (e *Engine) readInfo(ctx context.Context, mc MemberConfig) memberInfo {
	in := memberInfo{id: mc.ID}
	c := e.mc[mc.ID]
	s, err := c.Info(ctx, "replication", "clients").Result()
	if err != nil {
		in.fields = [][2]string{{"error", short(err.Error(), 120)}}
		return in
	}
	in.up = true
	in.conns, _ = strconv.ParseFloat(field(s, "connected_clients", "0"), 64)
	in.blocked, _ = strconv.ParseFloat(field(s, "blocked_clients", "0"), 64)
	role := field(s, "role", "")
	if role == "master" {
		in.primary = true
		in.fields = [][2]string{{"role", "master"}, {"connected_slaves", field(s, "connected_slaves", "0")}, {"master_repl_offset", field(s, "master_repl_offset", "")}}
		return in
	}
	in.master = field(s, "master_host", "") + ":" + field(s, "master_port", "")
	in.link = field(s, "master_link_status", "")
	behind := ""
	if p, err := strconv.ParseInt(field(s, "master_repl_offset", "0"), 10, 64); err == nil {
		if r, err := strconv.ParseInt(field(s, "slave_repl_offset", "0"), 10, 64); err == nil {
			behind = strconv.FormatInt(p-r, 10)
		}
	}
	in.fields = [][2]string{
		{"role", "slave"},
		{"master_host", in.master},
		{"master_link_status", in.link},
		{"master_last_io_seconds_ago", field(s, "master_last_io_seconds_ago", "")},
		{"slave_repl_offset", field(s, "slave_repl_offset", "")},
		{"received, not applied (bytes)", nz(behind, "—")},
	}
	return in
}

// ensureHeartbeatKeys picks, for each primary, a key that hashes to a slot it
// owns. Outside cluster mode there is one primary and one key.
func (e *Engine) ensureHeartbeatKeys(ctx context.Context, primaries []string) {
	e.mu.Lock()
	topo := e.topology
	missing := []string{}
	for _, p := range primaries {
		if e.hbKeys[p] == "" {
			missing = append(missing, p)
		}
	}
	e.mu.Unlock()
	if len(missing) == 0 {
		return
	}
	if topo != "cluster" {
		e.mu.Lock()
		e.hbKeys = map[string]string{primaries[0]: "perfsim:hb"}
		e.mu.Unlock()
		return
	}
	cc, ok := e.writer.(*redis.ClusterClient)
	if !ok {
		return
	}
	for _, p := range missing {
		c := e.mc[p]
		for n := 0; n < 5000; n++ {
			key := "perfsim:hb:{" + strconv.Itoa(n) + "}"
			node, err := cc.MasterForKey(ctx, key)
			if err != nil {
				break
			}
			if node.Options().Addr == c.Options().Addr || e.sameMember(ctx, node.Options().Addr, p) {
				e.mu.Lock()
				e.hbKeys[p] = key
				e.mu.Unlock()
				break
			}
		}
	}
}

func (e *Engine) sameMember(ctx context.Context, addr, id string) bool {
	host, port, _ := strings.Cut(addr, ":")
	m := e.memberCfg(id)
	if strconv.Itoa(m.Port) != port {
		return false
	}
	return e.memberIPs(ctx)[id] == host || m.Host == host
}

func (e *Engine) memberCfg(id string) MemberConfig {
	for _, m := range e.cfg.Members {
		if m.ID == id {
			return m
		}
	}
	return MemberConfig{}
}

var ipCache sync.Map

// memberIPs resolves each member's host name, since Valkey reports addresses.
func (e *Engine) memberIPs(ctx context.Context) map[string]string {
	out := map[string]string{}
	for _, m := range e.cfg.Members {
		if v, ok := ipCache.Load(m.Host); ok {
			out[m.ID] = v.(string)
			continue
		}
		if addrs, err := net.DefaultResolver.LookupHost(ctx, m.Host); err == nil && len(addrs) > 0 {
			ipCache.Store(m.Host, addrs[0])
			out[m.ID] = addrs[0]
		}
	}
	return out
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
