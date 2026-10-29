package pgsql

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"perfsim/internal/core"
)

type memberSample struct {
	m       core.Member
	srv     core.ServerSample
	primary bool
}

func (e *Engine) Sample(ctx context.Context) ([]core.Member, core.ServerSample) {
	out := make([]memberSample, len(e.cfg.Members))
	var wg sync.WaitGroup
	for i, mc := range e.cfg.Members {
		wg.Add(1)
		go func(i int, mc MemberConfig) {
			defer wg.Done()
			out[i] = e.sampleMember(ctx, mc)
		}(i, mc)
	}
	wg.Wait()

	primary := ""
	for _, s := range out {
		if s.primary && primary == "" {
			primary = s.m.ID
		}
	}
	if primary != "" {
		e.followPrimary(primary)
	}
	e.mu.Lock()
	if primary != "" {
		e.writerID = primary
	}
	writer := e.writerID
	for _, s := range out {
		if s.m.Lag >= 0 {
			e.lag[s.m.ID] = s.m.Lag
		} else {
			delete(e.lag, s.m.ID)
		}
	}
	e.mu.Unlock()

	var members []core.Member
	var srv core.ServerSample
	for _, s := range out {
		if s.m.ID == writer {
			srv = s.srv
		}
		members = append(members, s.m)
	}
	return members, srv
}

func (e *Engine) sampleMember(ctx context.Context, mc MemberConfig) memberSample {
	d := e.mdb[mc.ID]
	s := memberSample{m: core.Member{ID: mc.ID, Role: mc.Role, Lag: -1}}
	var rec bool
	if err := d.QueryRowContext(ctx, "SELECT pg_is_in_recovery()").Scan(&rec); err != nil {
		s.m.Chip, s.m.ChipText = "bad", "unreachable"
		s.m.Fields = [][2]string{{"error", short(err.Error(), 120)}}
		return s
	}
	s.m.Up = true
	var lag sql.NullFloat64
	if err := d.QueryRowContext(ctx, "SELECT EXTRACT(EPOCH FROM (clock_timestamp() - ts))::float8 FROM heartbeat WHERE id = 1").Scan(&lag); err == nil && lag.Valid {
		s.m.Lag = lag.Float64
		if s.m.Lag < 0 {
			s.m.Lag = 0
		}
	}
	var active, total, locks float64
	d.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE state = 'active'), count(*), count(*) FILTER (WHERE wait_event_type = 'Lock')
		FROM pg_stat_activity WHERE backend_type = 'client backend'`).Scan(&active, &total, &locks)
	s.srv = core.ServerSample{Threads: active, Conns: total, LockWaits: locks}

	if !rec {
		s.primary, s.m.Role, s.m.Lag = true, "primary", 0
		var streaming int
		var syncNames, walLevel string
		var standbys sql.NullString
		d.QueryRowContext(ctx, `SELECT count(*), string_agg(COALESCE(application_name, '') || ' ' || state || ' ' || sync_state, ', ')
			FROM pg_stat_replication`).Scan(&streaming, &standbys)
		d.QueryRowContext(ctx, "SELECT current_setting('synchronous_standby_names'), current_setting('wal_level')").Scan(&syncNames, &walLevel)
		s.m.Fields = [][2]string{
			{"pg_is_in_recovery()", "false"},
			{"standbys streaming", strconv.Itoa(streaming)},
			{"pg_stat_replication", short(nz(standbys.String, "none"), 120)},
			{"synchronous_standby_names", nz(syncNames, "''")},
			{"wal_level", walLevel},
		}
		s.m.Chip, s.m.ChipText = "ok", "primary"
		return s
	}

	s.m.Role = "standby"
	var paused bool
	var recvLSN, replayLSN, applyDelay, standbyDelay string
	var behind sql.NullFloat64
	var status, sender sql.NullString
	d.QueryRowContext(ctx, `SELECT pg_is_wal_replay_paused(),
			COALESCE(pg_last_wal_receive_lsn()::text, ''), COALESCE(pg_last_wal_replay_lsn()::text, ''),
			pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn())::float8,
			current_setting('recovery_min_apply_delay'), current_setting('max_standby_streaming_delay')`).
		Scan(&paused, &recvLSN, &replayLSN, &behind, &applyDelay, &standbyDelay)
	d.QueryRowContext(ctx, "SELECT status, sender_host FROM pg_stat_wal_receiver LIMIT 1").Scan(&status, &sender)
	s.m.Fields = [][2]string{
		{"WAL receiver", nz(status.String, "not running")},
		{"sender_host", nz(sender.String, "—")},
		{"pg_is_wal_replay_paused()", strconv.FormatBool(paused)},
		{"received, not replayed", fmtBytes(behind.Float64)},
		{"pg_last_wal_receive_lsn()", recvLSN},
		{"pg_last_wal_replay_lsn()", replayLSN},
		{"recovery_min_apply_delay", applyDelay},
		{"max_standby_streaming_delay", standbyDelay},
	}
	switch {
	case paused:
		s.m.Chip, s.m.ChipText = "bad", "replay paused"
	case status.String != "streaming":
		s.m.Chip, s.m.ChipText = "bad", "not streaming"
	case s.m.Lag > 30:
		s.m.Chip, s.m.ChipText = "bad", "lagging"
	case s.m.Lag > 5:
		s.m.Chip, s.m.ChipText = "warn", "behind"
	default:
		s.m.Chip, s.m.ChipText = "ok", "in sync"
	}
	return s
}

func fmtBytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", b/(1<<10))
	}
	return fmt.Sprintf("%.0f B", b)
}

func nz(s, def string) string {
	if strings.TrimSpace(s) == "" {
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
