package dev.dbcanvas.ledgersim.sim;

/**
 * What the workload does and how hard. Every field is live-editable; the workers
 * read the current instance at the top of each transaction, so a change takes
 * effect within one operation rather than at the next restart.
 *
 * <p>The mix percentages are relative weights, not a distribution that has to add
 * up — normalising them here means a user can set "reads 200" without having to
 * recompute the other three.
 */
public final class Knobs {
    public int threads = 8;
    /** Operations per second across all workers; 0 means as fast as the pool allows. */
    public int ratePerSec = 0;

    /** "" = whatever the pool/driver default is. Otherwise a java.sql.Connection constant name. */
    public String isolation = "";

    public int linesPerOrder = 3;
    /** Customer accounts to seed. Only read at seed time. */
    public int customers = 2_000;
    /** 1..Schema.REVENUE_SHARDS — the contention knob. 1 serialises everything on one row. */
    public int revenueShards = 8;

    /** Share of transactions aimed at a small set of accounts, 0..1. */
    public double hotAccountShare = 0.0;
    public int hotAccountCount = 10;

    /** Share of place-order transactions that take their two row locks in reverse order, 0..1. */
    public double deadlockShare = 0.0;

    /** Per-session lock wait timeout; 0 leaves the server's own setting alone. */
    public int lockWaitSeconds = 0;

    /** How many times a deadlocked transaction is retried before it is counted as an error. */
    public int maxRetries = 3;

    // Relative weights of the four operations.
    public int weightPlace = 50;
    public int weightSettle = 20;
    public int weightRefund = 5;
    public int weightRead = 25;

    public Knobs copy() {
        Knobs k = new Knobs();
        k.threads = threads;
        k.ratePerSec = ratePerSec;
        k.isolation = isolation;
        k.linesPerOrder = linesPerOrder;
        k.customers = customers;
        k.revenueShards = revenueShards;
        k.hotAccountShare = hotAccountShare;
        k.hotAccountCount = hotAccountCount;
        k.deadlockShare = deadlockShare;
        k.lockWaitSeconds = lockWaitSeconds;
        k.maxRetries = maxRetries;
        k.weightPlace = weightPlace;
        k.weightSettle = weightSettle;
        k.weightRefund = weightRefund;
        k.weightRead = weightRead;
        return k;
    }

    public int totalWeight() {
        int t = Math.max(0, weightPlace) + Math.max(0, weightSettle)
                + Math.max(0, weightRefund) + Math.max(0, weightRead);
        return t == 0 ? 1 : t;
    }

    /** Clamps everything to a range the workload can actually honour. */
    public Knobs sane() {
        threads = clamp(threads, 1, 256);
        ratePerSec = clamp(ratePerSec, 0, 1_000_000);
        linesPerOrder = clamp(linesPerOrder, 1, 200);
        customers = clamp(customers, 1, 5_000_000);
        revenueShards = clamp(revenueShards, 1, dev.dbcanvas.ledgersim.db.Schema.REVENUE_SHARDS);
        hotAccountShare = clamp(hotAccountShare, 0.0, 1.0);
        hotAccountCount = clamp(hotAccountCount, 1, 100_000);
        deadlockShare = clamp(deadlockShare, 0.0, 1.0);
        lockWaitSeconds = clamp(lockWaitSeconds, 0, 3600);
        maxRetries = clamp(maxRetries, 0, 20);
        weightPlace = clamp(weightPlace, 0, 1000);
        weightSettle = clamp(weightSettle, 0, 1000);
        weightRefund = clamp(weightRefund, 0, 1000);
        weightRead = clamp(weightRead, 0, 1000);
        return this;
    }

    private static int clamp(int v, int lo, int hi) { return Math.max(lo, Math.min(hi, v)); }
    private static double clamp(double v, double lo, double hi) { return Math.max(lo, Math.min(hi, v)); }
}
