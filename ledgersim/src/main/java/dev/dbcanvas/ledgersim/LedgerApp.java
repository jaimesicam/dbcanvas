package dev.dbcanvas.ledgersim;

import dev.dbcanvas.ledgersim.conn.ConnSpec;
import dev.dbcanvas.ledgersim.conn.DriverKind;
import dev.dbcanvas.ledgersim.conn.Engine;
import dev.dbcanvas.ledgersim.conn.JdbcUrl;
import dev.dbcanvas.ledgersim.conn.PoolManager;
import dev.dbcanvas.ledgersim.conn.PoolSpec;
import dev.dbcanvas.ledgersim.db.Bootstrap;
import dev.dbcanvas.ledgersim.db.Dialect;
import dev.dbcanvas.ledgersim.db.LedgerRepo;
import dev.dbcanvas.ledgersim.db.Schema;
import dev.dbcanvas.ledgersim.db.Seeder;
import dev.dbcanvas.ledgersim.sim.Knobs;
import dev.dbcanvas.ledgersim.sim.Workload;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.sql.Connection;
import java.sql.SQLException;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Everything the process owns, and the one place a configuration change is
 * applied in the right order.
 *
 * <p>The order matters and is easy to get wrong: stop the workers, swap the pool
 * (which proves the new configuration before the old one is dropped), rebuild the
 * dialect if the engine changed, make sure the schema exists on whatever we are
 * now pointed at, and only then start the workers again. Getting that wrong
 * produces the two failure modes this class exists to prevent — workers holding
 * connections from a pool that is being closed, and a workload issuing MySQL
 * syntax at PostgreSQL for the first second after a switch.
 */
public final class LedgerApp implements AutoCloseable {
    private static final Logger log = LoggerFactory.getLogger(LedgerApp.class);

    /** What DBCanvas detected and passed in at deploy — never mutated, so "reset" means something. */
    private final ConnSpec detected;
    private final PoolSpec detectedPool;
    private final Knobs detectedKnobs;
    private final String label;

    private final PoolManager pool;
    private volatile Dialect dialect;
    private volatile Workload workload;
    private volatile String lastError = "";

    public LedgerApp(ConnSpec spec, PoolSpec poolSpec, Knobs knobs, String label) {
        this.detected = spec.copy();
        this.detectedPool = poolSpec.copy();
        this.detectedKnobs = knobs.copy();
        this.label = label;
        this.pool = new PoolManager(spec, poolSpec);
        this.dialect = new Dialect(spec.engine);
        this.workload = new Workload(pool, dialect);
        this.workload.setKnobs(knobs.copy());
    }

    public PoolManager pool() { return pool; }
    public Workload workload() { return workload; }
    public String label() { return label; }
    public ConnSpec detected() { return detected.copy(); }

    /** Brings the pool up, creates the schema and seeds. Safe to call repeatedly. */
    public synchronized void initialise(ConnSpec spec, PoolSpec poolSpec) throws SQLException {
        // A JDBC URL names a database that has to exist already; a node pointed at
        // a fresh server would otherwise stop at "database does not exist".
        Bootstrap.ensureDatabase(spec);
        pool.apply(spec, poolSpec);
        dialect = new Dialect(spec.engine);
        try (Connection c = pool.connection()) {
            Schema.create(c, dialect);
            long accounts = Seeder.seed(c, dialect, workload.knobs().customers);
            workload.setAccountCount(accounts);
        }
        lastError = "";
    }

    /**
     * Points the simulator at a different configuration without restarting the
     * process. Throws with the driver's own message if the new configuration
     * cannot connect, in which case nothing has changed and the workload is left
     * running against what it had.
     */
    public synchronized void reconfigure(ConnSpec spec, PoolSpec poolSpec) throws SQLException {
        boolean wasRunning = workload.running();
        workload.stop();
        try {
            Engine before = dialect.engine;
            Bootstrap.ensureDatabase(spec);
            pool.apply(spec, poolSpec);
            if (spec.engine != before) {
                dialect = new Dialect(spec.engine);
                Knobs keep = workload.knobs();
                workload.close();
                workload = new Workload(pool, dialect);
                workload.setKnobs(keep);
            }
            try (Connection c = pool.connection()) {
                Schema.create(c, dialect);
                long accounts = new LedgerRepo(dialect).countRows(c, "accounts");
                if (accounts < Schema.REVENUE_SHARDS + 1L) {
                    accounts = Seeder.seed(c, dialect, workload.knobs().customers);
                }
                workload.setAccountCount(accounts);
            }
            // Every successful swap starts a new measurement window — see
            // Workload.resetMetrics for why this is not merely tidiness.
            workload.resetMetrics();
            lastError = "";
        } finally {
            if (wasRunning) workload.start();
        }
    }

    /** Reseeds to the current customer count without dropping what is already there. */
    public synchronized long seed() throws SQLException {
        try (Connection c = pool.connection()) {
            long n = Seeder.seed(c, dialect, workload.knobs().customers);
            workload.setAccountCount(n);
            return n;
        }
    }

    /** The connection panel's data: what was detected, what is effective, and why. */
    public Map<String, Object> connectionState() {
        ConnSpec cur = pool.spec();
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("engine", cur.engine.id);
        m.put("driver", cur.driver.id);
        m.put("driverLabel", cur.driver.label);
        m.put("driverLicense", cur.driver.license);
        m.put("hosts", cur.hosts);
        m.put("port", cur.effectivePort());
        m.put("database", cur.database);
        m.put("user", cur.user);
        m.put("tls", cur.tls);
        m.put("props", cur.props);
        m.put("urlOverride", cur.urlOverride);
        m.put("url", PoolManager.redact(JdbcUrl.build(cur)));
        m.put("autoProps", JdbcUrl.autoProps(cur));
        m.put("effectiveProps", JdbcUrl.effectiveProps(cur));
        m.put("notes", JdbcUrl.notes(cur));
        m.put("detectedUrl", PoolManager.redact(JdbcUrl.build(detected)));
        m.put("modified", !JdbcUrl.build(detected).equals(JdbcUrl.build(cur))
                || !detected.driver.equals(cur.driver));
        m.put("pool", poolState());
        m.put("catalog", catalog());
        return m;
    }

    private Map<String, Object> poolState() {
        PoolSpec p = pool.poolSpec();
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("mode", p.mode);
        m.put("maximumPoolSize", p.maximumPoolSize);
        m.put("minimumIdle", p.minimumIdle);
        m.put("connectionTimeoutMs", p.connectionTimeoutMs);
        m.put("idleTimeoutMs", p.idleTimeoutMs);
        m.put("maxLifetimeMs", p.maxLifetimeMs);
        m.put("keepaliveTimeMs", p.keepaliveTimeMs);
        m.put("validationTimeoutMs", p.validationTimeoutMs);
        m.put("leakDetectionThresholdMs", p.leakDetectionThresholdMs);
        m.put("initializationFailTimeoutMs", p.initializationFailTimeoutMs);
        m.put("autoCommit", p.autoCommit);
        m.put("readOnly", p.readOnly);
        m.put("transactionIsolation", p.transactionIsolation);
        m.put("connectionInitSql", p.connectionInitSql);
        m.put("poolName", p.poolName);
        return m;
    }

    /** What the UI offers: the engines, and which drivers are on the classpath for each. */
    public static List<Map<String, Object>> catalog() {
        List<Map<String, Object>> out = new ArrayList<>();
        for (Engine e : Engine.values()) {
            Map<String, Object> m = new LinkedHashMap<>();
            m.put("id", e.id);
            m.put("label", e.label);
            m.put("defaultPort", e.defaultPort);
            List<Map<String, Object>> drivers = new ArrayList<>();
            for (DriverKind d : DriverKind.forEngine(e)) {
                Map<String, Object> dm = new LinkedHashMap<>();
                dm.put("id", d.id);
                dm.put("label", d.label);
                dm.put("scheme", d.urlScheme);
                dm.put("license", d.license);
                dm.put("available", d.available());
                drivers.add(dm);
            }
            m.put("drivers", drivers);
            out.add(m);
        }
        return out;
    }

    /** Row counts and the balance check, read straight off the database. */
    public Map<String, Object> dataset() {
        Map<String, Object> m = new LinkedHashMap<>();
        try (Connection c = pool.connection()) {
            LedgerRepo repo = new LedgerRepo(dialect);
            m.put("accounts", repo.countRows(c, "accounts"));
            m.put("orders", repo.countRows(c, "orders"));
            m.put("orderLines", repo.countRows(c, "order_lines"));
            m.put("ledgerEntries", repo.countRows(c, "ledger_entries"));
            long balance = repo.balanceCheck(c);
            m.put("balanceCheckMinor", balance);
            // Zero is the only correct answer. Anything else means a transaction was
            // observed half-applied, which is a finding, not a rounding error.
            m.put("balanced", balance == 0);
        } catch (SQLException e) {
            m.put("error", e.getMessage());
            m.put("sqlState", e.getSQLState());
        }
        return m;
    }

    public String lastError() { return lastError; }
    public void setLastError(String e) { lastError = e == null ? "" : e; }

    @Override
    public void close() {
        workload.close();
        pool.close();
    }
}
