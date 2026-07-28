package sim

import (
	"database/sql"
	"sync/atomic"
)

// MemberPool holds independent connections to specific PXC cluster members —
// new for MarketChaos; no prior sim distinguishes "a specific node" from
// "the cluster" (every other sim's Store only ever holds the one connection
// dbcanvas resolved). Only populated when linked directly to a PXC cluster
// frame (TARGET_KIND=="pxc"); every other target shape (including a direct
// single "pxcnode" link, which is deliberately the opposite case — an app
// that never spreads writes at all) leaves this nil.
//
// This is what lets the Institutional Trader agent (see agents_trading.go)
// pin different workers to different members on purpose, producing genuine
// cross-node Galera certification conflicts on a hot symbol — the mechanism
// the PXC-specific challenge pack (stage S4+) needs, in place of "multi-writer
// through HAProxy" (which this repo's single-writer HAProxy+PXC config can't
// represent; see the written plan's §5.3 design note).
type MemberPool struct {
	dbs  []*sql.DB
	next atomic.Uint64
}

// NewMemberPool wraps already-opened per-member connections. Ownership of
// each *sql.DB transfers to the pool — Close() closes all of them.
func NewMemberPool(dbs []*sql.DB) *MemberPool {
	return &MemberPool{dbs: dbs}
}

// Len returns how many members are available (0 for every non-"pxc" target).
func (p *MemberPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.dbs)
}

// Next round-robins across members — successive calls from any number of
// concurrent goroutines cycle through every member roughly evenly, which is
// exactly what's needed to keep concurrent institutional-trader workers
// spread across different nodes rather than piling onto one.
func (p *MemberPool) Next() *sql.DB {
	if p.Len() == 0 {
		return nil
	}
	i := p.next.Add(1) - 1
	return p.dbs[i%uint64(len(p.dbs))]
}

// At returns a specific member's pool by index, wrapping — used to pin one
// worker to one member for its whole lifetime (see agents_trading.go)
// instead of round-robining per-operation.
func (p *MemberPool) At(i int) *sql.DB {
	if p.Len() == 0 {
		return nil
	}
	return p.dbs[i%len(p.dbs)]
}

func (p *MemberPool) Close() {
	if p == nil {
		return
	}
	for _, db := range p.dbs {
		db.Close()
	}
}
