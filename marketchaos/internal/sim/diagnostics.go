package sim

import (
	"context"
	"time"

	"marketchaos/internal/store"
)

// ServerStatsView is GLOBAL STATUS turned into the rates/ratios the
// database-performance panel actually shows — derived from two consecutive
// point-in-time ServerStats reads (see Engine.ServerStatsView), not a
// background sampler: unlike the leaderboard, nothing here needs a
// consistent fixed-width window, and the dashboard's own 2s poll already
// gives every consecutive pair of reads a reasonable, if slightly uneven,
// interval to derive a rate from.
type ServerStatsView struct {
	QPS                 float64 `json:"qps"`
	TPS                 float64 `json:"tps"`
	ReadWriteRatio      float64 `json:"readWriteRatio"`
	LockWaitsPerSec     float64 `json:"lockWaitsPerSec"`
	AvgLockWaitMs       float64 `json:"avgLockWaitMs"`
	Deadlocks           int64   `json:"deadlocks"`
	TmpTablesPerSec     float64 `json:"tmpTablesPerSec"`
	TmpDiskTablesPerSec float64 `json:"tmpDiskTablesPerSec"`
	SortRowsPerSec      float64 `json:"sortRowsPerSec"`
	BufferPoolHitRate   float64 `json:"bufferPoolHitRate"`
	ThreadsConnected    int64   `json:"threadsConnected"`
	ThreadsRunning      int64   `json:"threadsRunning"`
	MaxUsedConnections  int64   `json:"maxUsedConnections"`
	// PoolConfigured/PoolInUse/PoolIdle are Go's OWN client-side pool
	// accounting (sql.DB.Stats()) — deliberately reported alongside the
	// server-side ThreadsConnected/MaxUsedConnections and the dashboard's
	// own knowledge of configured worker counts (see WorkerCounts in
	// profile.go), so the "workers != MySQL connections" panel the product
	// spec asks for has its 3 genuinely distinct numbers in one place:
	// worker goroutines configured, this app's own pool's connections, and
	// the server's actual session count (which also includes every OTHER
	// app/tool talking to it).
	PoolConfigured int `json:"poolConfigured"`
	PoolInUse      int `json:"poolInUse"`
	PoolIdle       int `json:"poolIdle"`
}

func (e *Engine) ServerStatsView(ctx context.Context) (ServerStatsView, error) {
	cur, err := e.Store.ServerStats(ctx)
	if err != nil {
		return ServerStatsView{}, err
	}
	now := time.Now()

	e.statsMu.Lock()
	prev, prevAt := e.lastServerStats, e.lastServerStatsAt
	e.lastServerStats, e.lastServerStatsAt = cur, now
	e.statsMu.Unlock()

	st := e.Store.DB.Stats()
	view := ServerStatsView{
		ThreadsConnected: cur.ThreadsConnected, ThreadsRunning: cur.ThreadsRunning,
		MaxUsedConnections: cur.MaxUsedConnections,
		PoolConfigured:     st.MaxOpenConnections, PoolInUse: st.InUse, PoolIdle: st.Idle,
	}
	if prevAt.IsZero() {
		return view, nil // first read ever — no prior sample to derive a rate from
	}
	secs := now.Sub(prevAt).Seconds()
	if secs <= 0 {
		secs = 1
	}
	writes := (cur.ComInsert - prev.ComInsert) + (cur.ComUpdate - prev.ComUpdate) + (cur.ComDelete - prev.ComDelete)
	reads := cur.ComSelect - prev.ComSelect
	if writes > 0 {
		view.ReadWriteRatio = float64(reads) / float64(writes)
	}
	lockWaitsDelta := cur.InnodbRowLockWaits - prev.InnodbRowLockWaits
	if lockWaitsDelta > 0 {
		view.AvgLockWaitMs = float64(cur.InnodbRowLockTimeMs-prev.InnodbRowLockTimeMs) / float64(lockWaitsDelta)
	}
	reqDelta := cur.BufferPoolReadReqs - prev.BufferPoolReadReqs
	hitRate := 1.0
	if reqDelta > 0 {
		hitRate = 1 - float64(cur.BufferPoolReads-prev.BufferPoolReads)/float64(reqDelta)
	}
	view.QPS = float64(cur.Questions-prev.Questions) / secs
	view.TPS = float64(writes) / secs
	view.LockWaitsPerSec = float64(lockWaitsDelta) / secs
	view.Deadlocks = cur.InnodbDeadlocks
	view.TmpTablesPerSec = float64(cur.CreatedTmpTables-prev.CreatedTmpTables) / secs
	view.TmpDiskTablesPerSec = float64(cur.CreatedTmpDiskTables-prev.CreatedTmpDiskTables) / secs
	view.SortRowsPerSec = float64(cur.SortRows-prev.SortRows) / secs
	view.BufferPoolHitRate = hitRate
	return view, nil
}

// Wsrep passes through to the store layer — only meaningful on a "pxc"
// family target; SHOW STATUS LIKE 'wsrep_%' simply returns no rows
// otherwise, so this is safe to call unconditionally.
func (e *Engine) Wsrep(ctx context.Context) (store.WsrepStatus, error) {
	return e.Store.WsrepStatus(ctx)
}

// HAProxyStats fetches the linked HAProxy node's stats — HAProxyStatsURL is
// "" for every target shape except haproxy-pxc/haproxy-mysql (see
// app/marketchaos.go's HAPROXY_STATS_URL env var).
func (e *Engine) HAProxyStats(ctx context.Context) ([]store.HAProxyRow, error) {
	if e.HAProxyStatsURL == "" {
		return nil, nil
	}
	return store.FetchHAProxyStats(ctx, e.HAProxyStatsURL)
}

func (e *Engine) Processlist(ctx context.Context) ([]store.ProcessRow, error) {
	return e.Store.Processlist(ctx)
}
func (e *Engine) LockWaits(ctx context.Context) ([]store.LockWaitRow, error) {
	return e.Store.LockWaits(ctx)
}
func (e *Engine) TableSizes(ctx context.Context) ([]store.TableSizeRow, error) {
	return e.Store.TableSizes(ctx)
}
func (e *Engine) Explain(ctx context.Context, sql string) ([]store.ExplainRow, error) {
	return e.Store.Explain(ctx, sql)
}

func (e *Engine) Leaderboard() []LeaderboardRow { return e.leaderboard.snapshot() }
