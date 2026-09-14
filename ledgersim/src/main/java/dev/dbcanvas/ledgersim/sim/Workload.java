package dev.dbcanvas.ledgersim.sim;

import dev.dbcanvas.ledgersim.conn.PoolManager;
import dev.dbcanvas.ledgersim.db.Dialect;
import dev.dbcanvas.ledgersim.db.LedgerRepo;
import dev.dbcanvas.ledgersim.db.Schema;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.sql.Connection;
import java.sql.SQLException;
import java.sql.Statement;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.ThreadLocalRandom;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * The worker pool that drives the ledger.
 *
 * <p>Each worker takes a connection per transaction rather than holding one, which
 * is what makes the HikariCP panel mean anything: pool exhaustion, connection
 * timeouts and the effect of maximumPoolSize are all visible precisely because
 * connections are borrowed and returned at the rate work arrives.
 *
 * <p>Deadlock handling is the other deliberate piece. A transaction chosen as a
 * victim is retried up to {@link Knobs#maxRetries} times, and both the deadlock
 * and each retry are counted — so the dashboard can show the thing an application
 * developer usually cannot: how much of the throughput is real work and how much
 * is the same work done again.
 */
public final class Workload implements AutoCloseable {
    private static final Logger log = LoggerFactory.getLogger(Workload.class);

    private final PoolManager pool;
    private final Dialect dialect;
    private final LedgerRepo repo;
    private volatile Metrics metrics = new Metrics();

    private volatile Knobs knobs = new Knobs();
    private volatile long accountCount = 0;
    private final AtomicBoolean running = new AtomicBoolean(false);
    private ExecutorService workers;

    public Workload(PoolManager pool, Dialect dialect) {
        this.pool = pool;
        this.dialect = dialect;
        this.repo = new LedgerRepo(dialect);
    }

    public Metrics metrics() { return metrics; }

    /**
     * Starts a fresh measurement window. Called on every successful
     * reconfiguration: a latency histogram that spans two drivers, or two
     * engines, describes neither of them, and "swap the driver and compare p95"
     * is the main thing this simulator is for.
     */
    public void resetMetrics() { metrics = new Metrics(); }
    public Knobs knobs() { return knobs.copy(); }
    public boolean running() { return running.get(); }
    public void setAccountCount(long n) { accountCount = n; }

    /** Applies new knobs, restarting the worker pool only when the thread count changed. */
    public synchronized void setKnobs(Knobs k) {
        Knobs next = k.sane();
        boolean resize = running.get() && next.threads != knobs.threads;
        knobs = next;
        if (resize) {
            stop();
            start();
        }
    }

    public synchronized void start() {
        if (!running.compareAndSet(false, true)) return;
        int n = knobs.threads;
        workers = Executors.newFixedThreadPool(n, r -> {
            Thread t = new Thread(r);
            t.setName("ledger-worker");
            t.setDaemon(true);
            return t;
        });
        for (int i = 0; i < n; i++) workers.submit(this::loop);
        log.info("workload started with {} workers", n);
    }

    public synchronized void stop() {
        if (!running.compareAndSet(true, false)) return;
        if (workers != null) {
            workers.shutdownNow();
            try {
                workers.awaitTermination(10, TimeUnit.SECONDS);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
            workers = null;
        }
        log.info("workload stopped");
    }

    private void loop() {
        while (running.get() && !Thread.currentThread().isInterrupted()) {
            Knobs k = knobs;
            long startedNanos = System.nanoTime();
            try {
                runOne(k);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                return;
            } catch (RuntimeException e) {
                // A pool that is mid-swap, or simply down, is not worth a hot loop.
                log.debug("worker iteration failed: {}", e.toString());
                sleepQuietly(250);
            }
            throttle(k, startedNanos);
        }
    }

    /** Spreads {@link Knobs#ratePerSec} across the workers by padding each iteration. */
    private void throttle(Knobs k, long startedNanos) {
        if (k.ratePerSec <= 0) return;
        long targetNanosPerOp = TimeUnit.SECONDS.toNanos(1) * Math.max(1, k.threads) / k.ratePerSec;
        long spent = System.nanoTime() - startedNanos;
        long remaining = targetNanosPerOp - spent;
        if (remaining > 0) sleepQuietly(TimeUnit.NANOSECONDS.toMillis(remaining) + 1);
    }

    private void runOne(Knobs k) throws InterruptedException {
        String op = pickOp(k);
        int attempt = 0;
        while (true) {
            long started = System.nanoTime();
            try (Connection c = pool.connection()) {
                prepareSession(c, k);
                c.setAutoCommit(false);
                try {
                    boolean did = execute(c, op, k);
                    c.commit();
                    if (did) metrics.recordCommit(op, (System.nanoTime() - started) / 1000);
                    else metrics.recordRollback(); // nothing to do: counted, not an error
                    return;
                } catch (SQLException e) {
                    safeRollback(c);
                    throw e;
                }
            } catch (SQLException e) {
                boolean deadlock = Dialect.isDeadlock(e);
                boolean lockTimeout = Dialect.isLockTimeout(e);
                if (deadlock) metrics.recordDeadlock();
                if (lockTimeout) metrics.recordLockTimeout();
                if ((deadlock || lockTimeout) && attempt < k.maxRetries) {
                    attempt++;
                    metrics.recordRetry();
                    // Back off a little and jitter, or the two victims simply collide again.
                    Thread.sleep(ThreadLocalRandom.current().nextInt(5, 25) * (attempt));
                    continue;
                }
                metrics.recordError(op, e);
                sleepQuietly(50);
                return;
            }
        }
    }

    private void prepareSession(Connection c, Knobs k) throws SQLException {
        if (k.isolation != null && !k.isolation.isBlank()) {
            Integer level = isolationLevel(k.isolation);
            if (level != null && c.getTransactionIsolation() != level) c.setTransactionIsolation(level);
        }
        if (k.lockWaitSeconds > 0) {
            try (Statement st = c.createStatement()) {
                st.execute(dialect.lockWaitStatement(k.lockWaitSeconds));
            }
        }
    }

    private boolean execute(Connection c, String op, Knobs k) throws SQLException {
        long customer = pickCustomer(k);
        switch (op) {
            case "place" -> {
                List<LedgerRepo.Line> lines = new ArrayList<>(k.linesPerOrder);
                ThreadLocalRandom rnd = ThreadLocalRandom.current();
                for (int i = 0; i < k.linesPerOrder; i++) {
                    lines.add(new LedgerRepo.Line(
                            "SKU-" + rnd.nextInt(1, 5000), rnd.nextInt(1, 6), rnd.nextLong(100, 50_000)));
                }
                boolean reversed = k.deadlockShare > 0
                        && ThreadLocalRandom.current().nextDouble() < k.deadlockShare;
                if (reversed) repo.placeOrderReversed(c, customer, k.revenueShards, lines);
                else repo.placeOrder(c, customer, k.revenueShards, lines);
                return true;
            }
            case "settle" -> {
                long id = repo.pickOpenOrder(c, 200);
                return id >= 0 && repo.settleOrder(c, id);
            }
            case "refund" -> {
                long id = repo.pickOpenOrder(c, 200);
                return id >= 0 && repo.refundOrder(c, id, k.revenueShards);
            }
            default -> {
                repo.statement(c, customer, 50);
                return true;
            }
        }
    }

    /**
     * Which customer this transaction touches. With hotAccountShare set, that
     * share of the traffic is aimed at the first hotAccountCount customers — the
     * shape of a real application where a few accounts are far busier than the
     * rest, and the reason a "the table is fine, one row is not" problem exists.
     */
    private long pickCustomer(Knobs k) {
        long n = accountCount > Schema.REVENUE_SHARDS
                ? accountCount - Schema.REVENUE_SHARDS
                : Math.max(1, k.customers);
        ThreadLocalRandom rnd = ThreadLocalRandom.current();
        if (k.hotAccountShare > 0 && rnd.nextDouble() < k.hotAccountShare) {
            long hot = Math.min(k.hotAccountCount, n);
            return Schema.FIRST_CUSTOMER_ID + rnd.nextLong(hot);
        }
        return Schema.FIRST_CUSTOMER_ID + rnd.nextLong(n);
    }

    private static String pickOp(Knobs k) {
        int r = ThreadLocalRandom.current().nextInt(k.totalWeight());
        if ((r -= Math.max(0, k.weightPlace)) < 0) return "place";
        if ((r -= Math.max(0, k.weightSettle)) < 0) return "settle";
        if ((r -= Math.max(0, k.weightRefund)) < 0) return "refund";
        return "read";
    }

    public static Integer isolationLevel(String name) {
        return switch (name.trim().toUpperCase()) {
            case "TRANSACTION_READ_UNCOMMITTED" -> Connection.TRANSACTION_READ_UNCOMMITTED;
            case "TRANSACTION_READ_COMMITTED" -> Connection.TRANSACTION_READ_COMMITTED;
            case "TRANSACTION_REPEATABLE_READ" -> Connection.TRANSACTION_REPEATABLE_READ;
            case "TRANSACTION_SERIALIZABLE" -> Connection.TRANSACTION_SERIALIZABLE;
            default -> null;
        };
    }

    private static void safeRollback(Connection c) {
        try {
            c.rollback();
        } catch (SQLException ignored) {
            // The connection is already gone; the pool will discard it.
        }
    }

    private static void sleepQuietly(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    @Override
    public void close() { stop(); }
}
