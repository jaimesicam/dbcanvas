package dev.dbcanvas.ledgersim.conn;

import com.zaxxer.hikari.HikariConfig;
import com.zaxxer.hikari.HikariDataSource;
import com.zaxxer.hikari.HikariPoolMXBean;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.sql.Connection;
import java.sql.DatabaseMetaData;
import java.sql.DriverManager;
import java.sql.SQLException;
import java.sql.Statement;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.Properties;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Owns the live HikariCP pool and replaces it without restarting the process.
 *
 * <p>Rebuilding rather than restarting is what makes the connection panel worth
 * having: changing a driver, a TLS mode or maximumPoolSize and seeing the effect
 * on a running workload within a second is a different exercise from redeploying
 * a container and losing the state you were comparing against.
 *
 * <p>The swap is deliberately conservative. A candidate pool is built and proven
 * — one real connection, one real statement — *before* the live reference moves.
 * If it cannot connect, the old pool is still serving and the caller gets the
 * driver's own error text. The old pool is closed on a delay rather than
 * immediately so transactions already in flight finish on the connection they
 * started on instead of dying mid-commit.
 */
public final class PoolManager implements AutoCloseable {
    private static final Logger log = LoggerFactory.getLogger(PoolManager.class);

    /** Grace before the superseded pool is closed; longer than any sim transaction. */
    private static final long DRAIN_SECONDS = 5;

    private final ScheduledExecutorService closer =
            Executors.newSingleThreadScheduledExecutor(r -> {
                Thread t = new Thread(r, "pool-closer");
                t.setDaemon(true);
                return t;
            });

    private volatile HikariDataSource ds;
    private volatile ConnSpec spec;
    private volatile PoolSpec pool;
    private volatile long generation;

    // The direct path has no pool to ask, so it keeps its own counters. These are
    // the whole point of offering the mode: connectsFor/connectNanos give the
    // per-transaction cost of establishing a connection, which is exactly what a
    // pool removes and what is otherwise invisible.
    private volatile Properties directProps = new Properties();
    private final AtomicLong connects = new AtomicLong();
    private final AtomicLong connectNanos = new AtomicLong();
    private final AtomicLong connectFailures = new AtomicLong();

    public PoolManager(ConnSpec spec, PoolSpec pool) {
        this.spec = spec.copy();
        this.pool = pool.copy();
    }

    public ConnSpec spec() { return spec.copy(); }
    public PoolSpec poolSpec() { return pool.copy(); }
    public long generation() { return generation; }
    /** Whether work can be done: a live pool, or a proven direct configuration. */
    public boolean up() { return pool.direct() ? spec != null && generation > 0 : ds != null && !ds.isClosed(); }

    /**
     * A connection for one unit of work.
     *
     * <p>In pooled mode this borrows from Hikari and {@code close()} returns it.
     * In direct mode it opens a real one and {@code close()} genuinely closes it —
     * which is the behaviour being measured, so nothing here caches or reuses
     * anything. The connect is timed, because in direct mode that time is part of
     * every transaction and is the difference the two modes are there to show.
     */
    public Connection connection() throws SQLException {
        if (pool.direct()) {
            String url = JdbcUrl.build(spec);
            long t0 = System.nanoTime();
            try {
                Connection c = DriverManager.getConnection(url, directProps);
                connectNanos.addAndGet(System.nanoTime() - t0);
                connects.incrementAndGet();
                return c;
            } catch (SQLException e) {
                connectFailures.incrementAndGet();
                throw e;
            }
        }
        HikariDataSource cur = ds;
        if (cur == null) throw new SQLException("connection pool is not started");
        return cur.getConnection();
    }

    /**
     * Builds a pool for the given configuration, proves it, and makes it live.
     * Throws with the driver's own message if the candidate cannot connect —
     * leaving whatever was already running untouched.
     */
    public synchronized void apply(ConnSpec newSpec, PoolSpec newPool) throws SQLException {
        if (newPool.direct()) {
            applyDirect(newSpec, newPool);
            return;
        }
        // Constructing a HikariDataSource with initializationFailTimeout > 0 opens a
        // connection there and then, and failure arrives as PoolInitializationException
        // — a RuntimeException, not a SQLException. Letting that escape means a caller
        // that handles SQLException (every caller here) sees nothing at all, so it is
        // unwrapped to the SQLException underneath it, which is where the SQLState and
        // the vendor code live.
        HikariDataSource candidate;
        try {
            candidate = new HikariDataSource(hikariConfig(newSpec, newPool));
        } catch (RuntimeException e) {
            throw asSQLException(e);
        }
        try (Connection c = candidate.getConnection(); Statement st = c.createStatement()) {
            st.execute("SELECT 1");
        } catch (SQLException | RuntimeException e) {
            candidate.close();
            throw e instanceof SQLException se ? se : asSQLException((RuntimeException) e);
        }

        HikariDataSource old = ds;
        ds = candidate;
        spec = newSpec.copy();
        pool = newPool.copy();
        generation++;
        log.info("pool generation {} live: {} via {}", generation, redactUrl(newSpec), newSpec.driver.label);

        resetDirectCounters();
        if (old != null) scheduleClose(old);
    }

    private void scheduleClose(HikariDataSource old) {
        closer.schedule(() -> {
            try {
                old.close();
            } catch (RuntimeException e) {
                log.warn("closing superseded pool: {}", e.toString());
            }
        }, DRAIN_SECONDS, TimeUnit.SECONDS);
    }

    private static HikariConfig hikariConfig(ConnSpec s, PoolSpec p) {
        HikariConfig c = new HikariConfig();
        c.setJdbcUrl(JdbcUrl.build(s));
        c.setDriverClassName(s.driver.className);
        if (s.user != null && !s.user.isEmpty()) c.setUsername(s.user);
        if (s.password != null && !s.password.isEmpty()) c.setPassword(s.password);

        c.setPoolName(blankTo(p.poolName, "ledgersim"));
        c.setMaximumPoolSize(Math.max(1, p.maximumPoolSize));
        if (p.minimumIdle >= 0) c.setMinimumIdle(p.minimumIdle);
        c.setConnectionTimeout(p.connectionTimeoutMs);
        c.setIdleTimeout(p.idleTimeoutMs);
        c.setMaxLifetime(p.maxLifetimeMs);
        c.setKeepaliveTime(p.keepaliveTimeMs);
        c.setValidationTimeout(p.validationTimeoutMs);
        c.setLeakDetectionThreshold(p.leakDetectionThresholdMs);
        c.setInitializationFailTimeout(p.initializationFailTimeoutMs);
        c.setAutoCommit(p.autoCommit);
        c.setReadOnly(p.readOnly);
        if (p.transactionIsolation != null && !p.transactionIsolation.isBlank()) {
            c.setTransactionIsolation(p.transactionIsolation.trim());
        }
        if (p.connectionInitSql != null && !p.connectionInitSql.isBlank()) {
            c.setConnectionInitSql(p.connectionInitSql.trim());
        }
        return c;
    }

    /**
     * Switches to the un-pooled path, proving the configuration first in exactly
     * the same way the pooled path does: one real connection, one real statement,
     * before anything live is replaced. A pool that was running is closed on the
     * same drain delay, so transactions in flight finish on the connection they
     * started on rather than dying mid-commit.
     */
    private void applyDirect(ConnSpec newSpec, PoolSpec newPool) throws SQLException {
        Properties props = new Properties();
        if (newSpec.user != null && !newSpec.user.isEmpty()) props.setProperty("user", newSpec.user);
        if (newSpec.password != null && !newSpec.password.isEmpty()) props.setProperty("password", newSpec.password);

        try {
            Class.forName(newSpec.driver.className);
        } catch (ClassNotFoundException e) {
            throw new SQLException("driver class not on the classpath: " + newSpec.driver.className, e);
        }
        String url = JdbcUrl.build(newSpec);
        try (Connection c = DriverManager.getConnection(url, props); Statement st = c.createStatement()) {
            st.execute("SELECT 1");
        }

        HikariDataSource old = ds;
        directProps = props;
        ds = null;
        spec = newSpec.copy();
        pool = newPool.copy();
        resetDirectCounters();
        generation++;
        log.info("pool generation {} live: DIRECT (no pool) {} via {}",
                generation, redactUrl(newSpec), newSpec.driver.label);
        if (old != null) scheduleClose(old);
    }

    private void resetDirectCounters() {
        connects.set(0);
        connectNanos.set(0);
        connectFailures.set(0);
    }

    /**
     * Tries a configuration without touching the live pool, and reports what
     * answered. Uses DriverManager rather than a throwaway Hikari pool because
     * the question being asked is about the URL and the credentials, and a pool
     * would only add its own failure modes to the answer.
     */
    public static Map<String, Object> probe(ConnSpec s, long timeoutMs) {
        Map<String, Object> out = new LinkedHashMap<>();
        String url = JdbcUrl.build(s);
        out.put("url", redact(url));
        out.put("driver", s.driver.id);

        Properties props = new Properties();
        if (s.user != null && !s.user.isEmpty()) props.setProperty("user", s.user);
        if (s.password != null && !s.password.isEmpty()) props.setProperty("password", s.password);

        int previous = DriverManager.getLoginTimeout();
        long started = System.nanoTime();
        try {
            Class.forName(s.driver.className);
            DriverManager.setLoginTimeout((int) Math.max(1, timeoutMs / 1000));
            try (Connection c = DriverManager.getConnection(url, props)) {
                DatabaseMetaData md = c.getMetaData();
                out.put("ok", true);
                out.put("serverVersion", md.getDatabaseProductName() + " " + md.getDatabaseProductVersion());
                out.put("driverVersion", md.getDriverName() + " " + md.getDriverVersion());
                out.put("autoCommit", c.getAutoCommit());
                out.put("isolation", isolationName(c.getTransactionIsolation()));
                out.put("readOnly", c.isReadOnly());
                try (Statement st = c.createStatement()) {
                    st.execute("SELECT 1");
                }
            }
        } catch (ClassNotFoundException e) {
            out.put("ok", false);
            out.put("error", "driver class not on the classpath: " + s.driver.className);
        } catch (SQLException e) {
            out.put("ok", false);
            // SQLState and vendor code are the two things that tell a driver problem
            // apart from a server problem, so they are never swallowed.
            out.put("error", e.getMessage());
            out.put("sqlState", e.getSQLState());
            out.put("vendorCode", e.getErrorCode());
        } finally {
            DriverManager.setLoginTimeout(previous);
            out.put("elapsedMs", (System.nanoTime() - started) / 1_000_000);
        }
        return out;
    }

    /**
     * Live connection counters. In pooled mode these come straight off Hikari's
     * own MXBean; in direct mode there is no pool to ask, so what is reported
     * instead is the thing that mode exists to expose — how many connections the
     * workload has opened, and what each one cost.
     *
     * <p>Deliberately no active/idle in direct mode. Counting in-flight
     * connections would mean wrapping every Connection to intercept close(), and
     * a reflection proxy on the hot path would tax exactly the latency the two
     * modes are being compared on. A number that distorts the measurement is
     * worse than no number.
     */
    public Map<String, Object> metrics() {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("generation", generation);
        m.put("driver", spec.driver.id);
        m.put("driverLabel", spec.driver.label);
        m.put("mode", pool.direct() ? "direct" : "pooled");

        if (pool.direct()) {
            long n = connects.get();
            m.put("up", up());
            m.put("connectionsOpened", n);
            m.put("connectFailures", connectFailures.get());
            // The headline number: what one connection costs, paid once per
            // transaction here and once per pool lifetime in the other mode.
            m.put("avgConnectMs", n == 0 ? 0.0 : Math.round(connectNanos.get() / (double) n / 10_000) / 100.0);
            return m;
        }

        HikariDataSource cur = ds;
        m.put("up", cur != null && !cur.isClosed());
        if (cur == null || cur.isClosed()) return m;
        try {
            HikariPoolMXBean b = cur.getHikariPoolMXBean();
            m.put("total", b.getTotalConnections());
            m.put("active", b.getActiveConnections());
            m.put("idle", b.getIdleConnections());
            m.put("awaitingConnection", b.getThreadsAwaitingConnection());
            m.put("maximumPoolSize", cur.getMaximumPoolSize());
            m.put("minimumIdle", cur.getMinimumIdle());
        } catch (RuntimeException e) {
            m.put("error", e.toString());
        }
        return m;
    }

    /** Digs the real SQLException out of a pool-initialisation failure, if there is one. */
    private static SQLException asSQLException(RuntimeException e) {
        for (Throwable t = e; t != null; t = t.getCause()) {
            if (t instanceof SQLException se) return se;
        }
        return new SQLException(e.getMessage() == null ? e.toString() : e.getMessage(), e);
    }

    public static String isolationName(int level) {
        return switch (level) {
            case Connection.TRANSACTION_READ_UNCOMMITTED -> "TRANSACTION_READ_UNCOMMITTED";
            case Connection.TRANSACTION_READ_COMMITTED -> "TRANSACTION_READ_COMMITTED";
            case Connection.TRANSACTION_REPEATABLE_READ -> "TRANSACTION_REPEATABLE_READ";
            case Connection.TRANSACTION_SERIALIZABLE -> "TRANSACTION_SERIALIZABLE";
            case Connection.TRANSACTION_NONE -> "TRANSACTION_NONE";
            default -> "UNKNOWN(" + level + ")";
        };
    }

    private static String redactUrl(ConnSpec s) { return redact(JdbcUrl.build(s)); }

    /** A URL may carry a password in its query string when it was pasted as an override. */
    public static String redact(String url) {
        return url == null ? "" : url.replaceAll("(?i)([?&](password|pwd)=)[^&]*", "$1****");
    }

    private static String blankTo(String v, String fallback) {
        return v == null || v.isBlank() ? fallback : v.trim();
    }

    @Override
    public void close() {
        closer.shutdownNow();
        HikariDataSource cur = ds;
        if (cur != null) cur.close();
    }
}
