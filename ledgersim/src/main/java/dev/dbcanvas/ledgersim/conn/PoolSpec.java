package dev.dbcanvas.ledgersim.conn;

/**
 * The HikariCP knobs, with Hikari's own defaults as the starting values.
 *
 * <p>These are exposed and live-editable because pool misconfiguration is one of
 * the most common things behind "the database is slow" that turns out not to be
 * the database: a maximumPoolSize far above the server's capacity to run
 * concurrent statements, a maxLifetime longer than the server's wait_timeout, a
 * connectionTimeout so long that a saturated pool looks like a hung query. Each
 * of those is reproducible here in a few seconds, against a real server.
 */
public final class PoolSpec {
    /**
     * "pooled" (HikariCP) or "direct" (a fresh connection per transaction, closed
     * after it, straight off DriverManager).
     *
     * <p>Direct is not a degraded mode — it is the other half of the experiment.
     * A great many real applications connect this way, deliberately or otherwise:
     * short-lived CLI jobs, cron scripts, anything on the classic PHP request
     * model, and most serverless handlers. Being able to run the identical
     * workload both ways against the same server is what turns "use a connection
     * pool" from advice into a measurement — and it is also how the cost a pool
     * hides (TCP, TLS, authentication, session setup, per transaction) becomes
     * visible instead of theoretical.
     *
     * <p>In direct mode every field below except transactionIsolation,
     * connectionInitSql, autoCommit and readOnly is inert: there is no pool for
     * them to configure. The panel says so rather than showing dead inputs.
     */
    public String mode = "pooled";

    public int maximumPoolSize = 10;
    /** -1 means "track maximumPoolSize", which is Hikari's own default behaviour. */
    public int minimumIdle = -1;
    public long connectionTimeoutMs = 30_000;
    public long idleTimeoutMs = 600_000;
    public long maxLifetimeMs = 1_800_000;
    public long keepaliveTimeMs = 0;
    public long validationTimeoutMs = 5_000;
    public long leakDetectionThresholdMs = 0;
    /** 1 = fail fast at startup; 0 = fail on first use; negative = never block. */
    public long initializationFailTimeoutMs = 1;
    public boolean autoCommit = true;
    public boolean readOnly = false;
    /** "" = driver default. Otherwise a java.sql.Connection constant name. */
    public String transactionIsolation = "";
    public String connectionInitSql = "";
    public String poolName = "ledgersim";

    /** Whether this spec asks for the un-pooled path. */
    public boolean direct() { return "direct".equalsIgnoreCase(mode == null ? "" : mode.trim()); }

    public PoolSpec copy() {
        PoolSpec p = new PoolSpec();
        p.mode = mode;
        p.maximumPoolSize = maximumPoolSize;
        p.minimumIdle = minimumIdle;
        p.connectionTimeoutMs = connectionTimeoutMs;
        p.idleTimeoutMs = idleTimeoutMs;
        p.maxLifetimeMs = maxLifetimeMs;
        p.keepaliveTimeMs = keepaliveTimeMs;
        p.validationTimeoutMs = validationTimeoutMs;
        p.leakDetectionThresholdMs = leakDetectionThresholdMs;
        p.initializationFailTimeoutMs = initializationFailTimeoutMs;
        p.autoCommit = autoCommit;
        p.readOnly = readOnly;
        p.transactionIsolation = transactionIsolation;
        p.connectionInitSql = connectionInitSql;
        p.poolName = poolName;
        return p;
    }
}
