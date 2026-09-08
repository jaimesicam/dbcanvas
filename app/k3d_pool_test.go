package main

import (
	"strings"
	"testing"
)

// claim turns a pool string into the interval form pickMetalLBBlock takes, so a test reads
// as the addresses a cluster holds rather than as arithmetic.
func claim(t *testing.T, pool string) [2]uint32 {
	t.Helper()
	lo, hi, ok := parseRange(pool)
	if !ok {
		t.Fatalf("bad pool in test: %q", pool)
	}
	return [2]uint32{lo, hi}
}

// Every K3D cluster in a stack sits on the same Docker network, so their MetalLB pools must
// not overlap: MetalLB is L2 here, and two speakers answering ARP for one address is a coin
// toss. Both clusters used to be handed the identical top-50 range.
func TestMetalLBBlocksDoNotCollide(t *testing.T) {
	const subnet = "172.21.0.0/16"
	first, err := pickMetalLBBlock(subnet, nil, "", 0)
	if err != nil {
		t.Fatalf("first cluster: %v", err)
	}
	if first != "172.21.255.246-172.21.255.253" {
		t.Errorf("first block = %q, want the top 8 below the broadcast", first)
	}
	second, err := pickMetalLBBlock(subnet, [][2]uint32{claim(t, first)}, "", 0)
	if err != nil {
		t.Fatalf("second cluster: %v", err)
	}
	if second != "172.21.255.238-172.21.255.245" {
		t.Errorf("second block = %q, want the next 8 down", second)
	}
	if first == second {
		t.Fatal("two clusters in one stack were handed the same pool")
	}
	// And a third, to be sure the walk keeps descending.
	third, err := pickMetalLBBlock(subnet, [][2]uint32{claim(t, first), claim(t, second)}, "", 0)
	if err != nil {
		t.Fatalf("third cluster: %v", err)
	}
	if third != "172.21.255.230-172.21.255.237" {
		t.Errorf("third block = %q", third)
	}
	// Each block is exactly k3dPoolSize addresses.
	lo, hi, _ := parseRange(second)
	if got := hi - lo + 1; got != k3dPoolSize {
		t.Errorf("block holds %d addresses, want %d", got, k3dPoolSize)
	}
}

// A redeploy must not move a cluster's addresses: its services, and anything pointed at
// them, keep the block it already had.
func TestMetalLBRedeployKeepsItsBlock(t *testing.T) {
	const subnet = "172.21.0.0/16"
	other := "172.21.255.246-172.21.255.253"
	mine := "172.21.255.238-172.21.255.245"
	got, err := pickMetalLBBlock(subnet, [][2]uint32{claim(t, other)}, mine, 0)
	if err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if got != mine {
		t.Errorf("redeploy moved the pool from %q to %q", mine, got)
	}
}

// A cluster deployed before blocks existed holds the old 50-address range. It keeps it —
// moving a running cluster's pool is worse than wasting addresses — and the next cluster
// has to step around the whole thing, which is why claims are compared by overlap and not
// by equality.
func TestMetalLBLegacyWideRangeIsHonoured(t *testing.T) {
	const subnet = "172.21.0.0/16"
	legacy := "172.21.255.204-172.21.255.253"
	if got, err := pickMetalLBBlock(subnet, nil, legacy, 0); err != nil || got != legacy {
		t.Errorf("legacy range: got %q, %v — want it kept", got, err)
	}
	next, err := pickMetalLBBlock(subnet, [][2]uint32{claim(t, legacy)}, "", 0)
	if err != nil {
		t.Fatalf("next cluster: %v", err)
	}
	lo, _, _ := parseRange(legacy)
	_, hi, _ := parseRange(next)
	if hi >= lo {
		t.Errorf("next block %q overlaps the legacy range %q", next, legacy)
	}
	// Blocks sit on a fixed grid measured down from the top of the subnet, so the first
	// one clear of a legacy range that is not grid-aligned leaves a few addresses unused
	// (.198-.203 here). That is the price of blocks always landing in the same places
	// whatever order clusters are deployed in.
	if next != "172.21.255.190-172.21.255.197" {
		t.Errorf("next block = %q, want the first grid block below the legacy range", next)
	}
}

// Running out of blocks must be an error the deploy reports, not a silent overlap.
func TestMetalLBExhaustionIsAnError(t *testing.T) {
	// /29 = 8 addresses: network, broadcast and six usable — no room for a block.
	if _, err := pickMetalLBBlock("10.0.0.0/29", nil, "", 0); err == nil {
		t.Error("a subnet too small for one block returned a pool")
	}
	// A /28 fits one block; a second cluster then has nowhere to go.
	one, err := pickMetalLBBlock("10.0.0.0/28", nil, "", 0)
	if err != nil {
		t.Fatalf("/28 first block: %v", err)
	}
	if _, err := pickMetalLBBlock("10.0.0.0/28", [][2]uint32{claim(t, one)}, "", 0); err == nil {
		t.Error("a second cluster was given a pool with no free block left")
	} else if !strings.Contains(err.Error(), "no free LoadBalancer block") {
		t.Errorf("unhelpful exhaustion error: %v", err)
	}
}

// A claim that cannot be parsed must not be read as "the whole subnet is free" or crash the
// walk — a config written by an older build could hold anything.
func TestMetalLBIgnoresUnparseableClaims(t *testing.T) {
	if _, _, ok := parseRange("not-a-range"); ok {
		t.Error("a nonsense pool parsed")
	}
	if _, _, ok := parseRange(""); ok {
		t.Error("an empty pool parsed")
	}
	got, err := pickMetalLBBlock("172.21.0.0/16", nil, "garbage", 0)
	if err != nil {
		t.Fatalf("with an unparseable previous range: %v", err)
	}
	if got != "172.21.255.246-172.21.255.253" {
		t.Errorf("expected a fresh block, got %q", got)
	}
}

// Frames provision concurrently, so when two clusters in one stack reach MetalLB at the same
// moment neither has recorded a range for the other to avoid. Reproduced live before this
// existed: both clusters advertised 172.23.255.246-172.23.255.253 on the same network. Each
// frame's position gives it a block without consulting anything shared.
func TestMetalLBConcurrentFramesGetDifferentBlocks(t *testing.T) {
	const subnet = "172.23.0.0/16"
	// Both deploy at once: no claims recorded anywhere yet.
	a, err := pickMetalLBBlock(subnet, nil, "", 0)
	if err != nil {
		t.Fatalf("frame 0: %v", err)
	}
	b, err := pickMetalLBBlock(subnet, nil, "", 1)
	if err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if a == b {
		t.Fatalf("two frames deploying together were handed the same pool %q", a)
	}
	alo, ahi, _ := parseRange(a)
	blo, bhi, _ := parseRange(b)
	if alo <= bhi && blo <= ahi {
		t.Errorf("%q and %q overlap", a, b)
	}
	if b != "172.23.255.238-172.23.255.245" {
		t.Errorf("second frame's block = %q", b)
	}
}

// The index is a position among the stack's K3D frames, by sorted id, so it does not move
// when a frame is relabelled or dragged around the canvas.
func TestK3DFrameIndexIsStable(t *testing.T) {
	doc := designDoc{Frames: []designFrame{
		{ID: "frame-b", Type: "k3d", Label: "zulu"},
		{ID: "frame-a", Type: "k3d", Label: "alpha"},
		{ID: "frame-pxc", Type: "pxc"}, // not a cluster; must not take a block
	}}
	if got := k3dFrameIndex(doc, "frame-a"); got != 0 {
		t.Errorf("frame-a index = %d, want 0", got)
	}
	if got := k3dFrameIndex(doc, "frame-b"); got != 1 {
		t.Errorf("frame-b index = %d, want 1", got)
	}
	// Reordering the design must not change either.
	doc.Frames[0], doc.Frames[1] = doc.Frames[1], doc.Frames[0]
	if got := k3dFrameIndex(doc, "frame-b"); got != 1 {
		t.Errorf("frame-b index moved to %d when the design was reordered", got)
	}
}

// A frame whose position maps onto a block another cluster already holds steps down to the
// first free one rather than colliding — the case where a cluster is added to a stack that
// was deployed before it existed.
func TestMetalLBIndexYieldsToAnExistingClaim(t *testing.T) {
	const subnet = "172.21.0.0/16"
	held := "172.21.255.238-172.21.255.245" // block 1, taken by a cluster already running
	got, err := pickMetalLBBlock(subnet, [][2]uint32{claim(t, held)}, "", 1)
	if err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if got == held {
		t.Fatal("took a block another cluster holds")
	}
	if got != "172.21.255.246-172.21.255.253" {
		t.Errorf("fell back to %q, want the highest free block", got)
	}
}
