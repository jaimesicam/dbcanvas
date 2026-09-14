package dev.dbcanvas.ledgersim.sim;

import java.sql.SQLException;
import java.util.ArrayDeque;
import java.util.Deque;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicLongArray;

/**
 * Counters and a latency histogram, plus the last few failures with their
 * SQLState and vendor code attached.
 *
 * <p>Keeping the raw SQLState is not housekeeping — it is the whole reason to run
 * two drivers against one server. "Connection reset" from one driver and
 * "08S01 / 0" from another are the same event described differently, and the
 * only way to make that visible is to stop paraphrasing the exception.
 */
public final class Metrics {
    /** Log-spaced buckets from 0.1 ms to ~100 s; index is floor(log2(micros)). */
    private static final int BUCKETS = 32;

    private final AtomicLongArray latency = new AtomicLongArray(BUCKETS);
    private final AtomicLong commits = new AtomicLong();
    private final AtomicLong rollbacks = new AtomicLong();
    private final AtomicLong deadlocks = new AtomicLong();
    private final AtomicLong lockTimeouts = new AtomicLong();
    private final AtomicLong retries = new AtomicLong();
    private final AtomicLong errors = new AtomicLong();
    private final AtomicLong latencySumMicros = new AtomicLong();

    private final long since = System.currentTimeMillis();
    private final Map<String, AtomicLong> byOp = new LinkedHashMap<>();
    private final Deque<Map<String, Object>> recentErrors = new ArrayDeque<>();

    public Metrics() {
        for (String op : new String[]{"place", "settle", "refund", "read"}) {
            byOp.put(op, new AtomicLong());
        }
    }

    public void recordCommit(String op, long micros) {
        commits.incrementAndGet();
        AtomicLong c = byOp.get(op);
        if (c != null) c.incrementAndGet();
        latencySumMicros.addAndGet(micros);
        latency.incrementAndGet(bucketOf(micros));
    }

    public void recordRollback() { rollbacks.incrementAndGet(); }
    public void recordRetry() { retries.incrementAndGet(); }
    public void recordDeadlock() { deadlocks.incrementAndGet(); }
    public void recordLockTimeout() { lockTimeouts.incrementAndGet(); }

    public void recordError(String op, SQLException e) {
        errors.incrementAndGet();
        Map<String, Object> row = new LinkedHashMap<>();
        row.put("at", System.currentTimeMillis());
        row.put("op", op);
        row.put("message", e.getMessage());
        row.put("sqlState", e.getSQLState());
        row.put("vendorCode", e.getErrorCode());
        row.put("class", e.getClass().getName());
        synchronized (recentErrors) {
            recentErrors.addFirst(row);
            while (recentErrors.size() > 25) recentErrors.removeLast();
        }
    }

    private static int bucketOf(long micros) {
        if (micros <= 0) return 0;
        int b = 63 - Long.numberOfLeadingZeros(micros);
        return Math.max(0, Math.min(BUCKETS - 1, b));
    }

    /** Approximate percentile from the histogram — bucket upper bound, in milliseconds. */
    private double percentileMs(double p) {
        long total = 0;
        for (int i = 0; i < BUCKETS; i++) total += latency.get(i);
        if (total == 0) return 0;
        long want = (long) Math.ceil(total * p);
        long seen = 0;
        for (int i = 0; i < BUCKETS; i++) {
            seen += latency.get(i);
            if (seen >= want) return Math.pow(2, i + 1) / 1000.0;
        }
        return Math.pow(2, BUCKETS) / 1000.0;
    }

    public Map<String, Object> snapshot() {
        Map<String, Object> m = new LinkedHashMap<>();
        long c = commits.get();
        m.put("since", since);
        m.put("windowMs", System.currentTimeMillis() - since);
        m.put("commits", c);
        m.put("rollbacks", rollbacks.get());
        m.put("deadlocks", deadlocks.get());
        m.put("lockTimeouts", lockTimeouts.get());
        m.put("retries", retries.get());
        m.put("errors", errors.get());
        m.put("avgMs", c == 0 ? 0.0 : round(latencySumMicros.get() / (double) c / 1000.0));
        m.put("p50Ms", round(percentileMs(0.50)));
        m.put("p95Ms", round(percentileMs(0.95)));
        m.put("p99Ms", round(percentileMs(0.99)));

        Map<String, Long> ops = new LinkedHashMap<>();
        byOp.forEach((k, v) -> ops.put(k, v.get()));
        m.put("ops", ops);

        synchronized (recentErrors) {
            m.put("recentErrors", new ArrayDeque<>(recentErrors).stream().toList());
        }
        return m;
    }

    private static double round(double v) { return Math.round(v * 100.0) / 100.0; }
}
