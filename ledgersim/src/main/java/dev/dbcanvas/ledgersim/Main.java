package dev.dbcanvas.ledgersim;

import dev.dbcanvas.ledgersim.conn.ConnSpec;
import dev.dbcanvas.ledgersim.conn.DriverKind;
import dev.dbcanvas.ledgersim.conn.Engine;
import dev.dbcanvas.ledgersim.conn.JdbcUrl;
import dev.dbcanvas.ledgersim.conn.PoolManager;
import dev.dbcanvas.ledgersim.conn.PoolSpec;
import dev.dbcanvas.ledgersim.http.Api;
import dev.dbcanvas.ledgersim.sim.Knobs;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.io.IOException;
import java.net.HttpURLConnection;
import java.net.URI;
import java.sql.SQLException;
import java.time.Duration;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Ledger Sim — DBCanvas's JDBC application simulator.
 *
 * <p>An order-and-payment ledger driven through JDBC and HikariCP, against MySQL
 * (Percona Server, PXC) or PostgreSQL. It exists to make the *client* half of a
 * database problem visible: which driver, which URL properties, which pool
 * settings, which isolation level — the things that differ between "it works
 * from the CLI" and "it fails from the application".
 *
 * <p>DBCanvas detects the target at deploy and passes it in as environment. The
 * dashboard shows the JDBC URL that was composed from it, explains every property
 * it derived, and lets all of it be edited and re-applied against the running
 * workload. See {@link JdbcUrl} and {@link PoolManager}.
 *
 * <h2>Environment</h2>
 * <pre>
 *   DB_ENGINE      mysql | postgres                      (default mysql)
 *   DB_DRIVER      mysql-connector-j | mariadb-connector-j | pgjdbc
 *   DB_HOST        host, or a comma-separated list for a multi-host URL
 *   DB_PORT        default 3306 / 5432
 *   DB_USER, DB_PASSWORD, DB_NAME
 *   DB_TLS         disable | prefer | require            (default prefer)
 *   DB_PARAMS      extra driver properties, "a=1&amp;b=2"
 *   JDBC_URL       full override; every DB_* field above becomes advisory
 *   POOL_MODE      pooled (HikariCP) | direct (a new connection per transaction)
 *   POOL_*         MAX, MIN_IDLE, CONNECTION_TIMEOUT_MS, IDLE_TIMEOUT_MS,
 *                  MAX_LIFETIME_MS, KEEPALIVE_MS, LEAK_DETECTION_MS, AUTOCOMMIT
 *   LS_*           THREADS, RATE, ISOLATION, CUSTOMERS, LINES, REVENUE_SHARDS,
 *                  HOT_SHARE, HOT_COUNT, DEADLOCK_SHARE, LOCK_WAIT_SECONDS
 *   LS_LABEL       what the dashboard calls the target
 *   LS_AUTOSTART   false to deploy idle                  (default true)
 *   PORT           dashboard port                        (default 8094)
 * </pre>
 */
public final class Main {
    private static final Logger log = LoggerFactory.getLogger(Main.class);
    private static final int DEFAULT_PORT = 8094;

    public static void main(String[] args) throws Exception {
        int port = envInt("PORT", DEFAULT_PORT);
        for (String a : args) {
            if ("-healthcheck".equals(a)) {
                System.exit(healthcheck(port) ? 0 : 1);
                return;
            }
            // Connect once using the environment, say what happened, and exit.
            // DBCanvas runs this in a throwaway container for the node form's
            // "Test connection", so a manual target can be checked before the
            // stack is deployed rather than after it has failed.
            if ("-testconn".equals(a)) {
                System.exit(testConnection() ? 0 : 1);
                return;
            }
        }

        ConnSpec spec = specFromEnv();
        PoolSpec pool = poolFromEnv();
        Knobs knobs = knobsFromEnv();
        String label = env("LS_LABEL", defaultLabel(spec));

        log.info("ledgersim starting: {} via {} ({}), connections: {}",
                PoolManager.redact(JdbcUrl.build(spec)), spec.driver.label, spec.driver.license,
                pool.direct() ? "DIRECT, one per transaction" : "HikariCP pool, max " + pool.maximumPoolSize);

        LedgerApp app = new LedgerApp(spec, pool, knobs, label);
        Runtime.getRuntime().addShutdownHook(new Thread(app::close, "shutdown"));

        // The dashboard comes up first and stays up even if the database never
        // does. A node that cannot reach its target is exactly when someone wants
        // to open the connection panel and change something — a process that exits
        // on a failed connection would take that away.
        new Api(app).start(port);

        connectWithRetry(app, spec, pool, Duration.ofMinutes(10));

        if (envBool("LS_AUTOSTART", true) && app.pool().up()) {
            app.workload().start();
        }
        Thread.currentThread().join();
    }

    /**
     * Keeps trying to bring the pool up. The target is usually a database node
     * being provisioned in parallel, so "connection refused" for the first minute
     * is normal rather than fatal.
     */
    private static void connectWithRetry(LedgerApp app, ConnSpec spec, PoolSpec pool, Duration limit) {
        long deadline = System.nanoTime() + limit.toNanos();
        long backoffMs = 1_000;
        while (System.nanoTime() < deadline) {
            try {
                app.initialise(spec, pool);
                log.info("connected; schema and accounts ready");
                return;
            } catch (SQLException e) {
                app.setLastError(e.getMessage());
                log.info("waiting for {}: {}", spec.hosts, e.getMessage());
                try {
                    Thread.sleep(backoffMs);
                } catch (InterruptedException ie) {
                    Thread.currentThread().interrupt();
                    return;
                }
                backoffMs = Math.min(backoffMs * 2, 15_000);
            }
        }
        log.warn("gave up connecting after {}; the dashboard is up and the connection can be changed there", limit);
    }

    // ------------------------------------------------------------------ env

    static ConnSpec specFromEnv() {
        ConnSpec s = new ConnSpec();
        Engine e = Engine.ofOrNull(env("DB_ENGINE", "mysql"));
        s.engine = e == null ? Engine.MYSQL : e;

        DriverKind d = DriverKind.ofOrNull(env("DB_DRIVER", ""));
        // A driver that cannot speak to the chosen engine is a misconfiguration, not
        // a preference — fall back rather than fail to start, and say so.
        if (d == null || d.engine != s.engine) {
            if (d != null) log.warn("DB_DRIVER={} cannot speak {}, using the default", d.id, s.engine.id);
            d = DriverKind.defaultFor(s.engine);
        }
        s.driver = d;

        s.hosts = Api.splitHosts(env("DB_HOST", ""));
        s.port = envInt("DB_PORT", 0);
        s.database = env("DB_NAME", "ledgersim");
        s.user = env("DB_USER", "");
        s.password = env("DB_PASSWORD", "");
        s.tls = env("DB_TLS", "prefer");
        s.urlOverride = env("JDBC_URL", "");
        s.props = parseParams(env("DB_PARAMS", ""));
        return s;
    }

    /** "a=1&b=2" as DBCanvas passes extra driver properties through. */
    static Map<String, String> parseParams(String raw) {
        Map<String, String> out = new LinkedHashMap<>();
        if (raw == null || raw.isBlank()) return out;
        for (String pair : raw.split("&")) {
            String t = pair.trim();
            if (t.isEmpty()) continue;
            int i = t.indexOf('=');
            if (i <= 0) out.put(t, "");
            else out.put(t.substring(0, i).trim(), t.substring(i + 1).trim());
        }
        return out;
    }

    static PoolSpec poolFromEnv() {
        PoolSpec p = new PoolSpec();
        String mode = env("POOL_MODE", p.mode).trim().toLowerCase();
        // An unrecognised value must not silently become "direct" — that would be
        // a node quietly running the other experiment.
        p.mode = "direct".equals(mode) ? "direct" : "pooled";
        p.maximumPoolSize = envInt("POOL_MAX", p.maximumPoolSize);
        p.minimumIdle = envInt("POOL_MIN_IDLE", p.minimumIdle);
        p.connectionTimeoutMs = envLong("POOL_CONNECTION_TIMEOUT_MS", p.connectionTimeoutMs);
        p.idleTimeoutMs = envLong("POOL_IDLE_TIMEOUT_MS", p.idleTimeoutMs);
        p.maxLifetimeMs = envLong("POOL_MAX_LIFETIME_MS", p.maxLifetimeMs);
        p.keepaliveTimeMs = envLong("POOL_KEEPALIVE_MS", p.keepaliveTimeMs);
        p.leakDetectionThresholdMs = envLong("POOL_LEAK_DETECTION_MS", p.leakDetectionThresholdMs);
        p.autoCommit = envBool("POOL_AUTOCOMMIT", p.autoCommit);
        return p;
    }

    static Knobs knobsFromEnv() {
        Knobs k = new Knobs();
        k.threads = envInt("LS_THREADS", k.threads);
        k.ratePerSec = envInt("LS_RATE", k.ratePerSec);
        k.isolation = env("LS_ISOLATION", k.isolation);
        k.customers = envInt("LS_CUSTOMERS", k.customers);
        k.linesPerOrder = envInt("LS_LINES", k.linesPerOrder);
        k.revenueShards = envInt("LS_REVENUE_SHARDS", k.revenueShards);
        k.hotAccountShare = envDouble("LS_HOT_SHARE", k.hotAccountShare);
        k.hotAccountCount = envInt("LS_HOT_COUNT", k.hotAccountCount);
        k.deadlockShare = envDouble("LS_DEADLOCK_SHARE", k.deadlockShare);
        k.lockWaitSeconds = envInt("LS_LOCK_WAIT_SECONDS", k.lockWaitSeconds);
        return k.sane();
    }

    private static String defaultLabel(ConnSpec s) {
        if (s.hosts.isEmpty()) return "external " + s.engine.id;
        return s.hosts.get(0) + ":" + s.effectivePort();
    }

    /**
     * One probe, printed as a line a human reads in a dialog. The driver's own
     * SQLState is included because it is the part that distinguishes "wrong
     * password" from "no route to host" from "this driver cannot authenticate
     * against this server", and those need different fixes.
     */
    private static boolean testConnection() {
        ConnSpec spec = specFromEnv();
        Map<String, Object> r = PoolManager.probe(spec, 8_000);
        boolean ok = Boolean.TRUE.equals(r.get("ok"));
        if (ok) {
            System.out.println("connected to " + r.get("serverVersion")
                    + " via " + r.get("driverVersion") + " in " + r.get("elapsedMs") + "ms");
        } else {
            Object state = r.get("sqlState");
            System.out.println("error: " + r.get("error")
                    + (state == null ? "" : " [SQLState " + state + "]")
                    + " — " + r.get("url"));
        }
        return ok;
    }

    /** Exits 0 only if the dashboard answers — the readiness check DBCanvas execs. */
    private static boolean healthcheck(int port) {
        try {
            HttpURLConnection c = (HttpURLConnection) URI.create(
                    "http://127.0.0.1:" + port + "/healthz").toURL().openConnection();
            c.setConnectTimeout(2_000);
            c.setReadTimeout(2_000);
            c.setRequestMethod("GET");
            return c.getResponseCode() == 200;
        } catch (IOException e) {
            return false;
        }
    }

    static String env(String k, String fallback) {
        String v = System.getenv(k);
        return v == null || v.isEmpty() ? fallback : v;
    }

    static int envInt(String k, int fallback) {
        try {
            return Integer.parseInt(env(k, String.valueOf(fallback)).trim());
        } catch (NumberFormatException e) {
            return fallback;
        }
    }

    static long envLong(String k, long fallback) {
        try {
            return Long.parseLong(env(k, String.valueOf(fallback)).trim());
        } catch (NumberFormatException e) {
            return fallback;
        }
    }

    static double envDouble(String k, double fallback) {
        try {
            return Double.parseDouble(env(k, String.valueOf(fallback)).trim());
        } catch (NumberFormatException e) {
            return fallback;
        }
    }

    static boolean envBool(String k, boolean fallback) {
        String v = env(k, "");
        if (v.isEmpty()) return fallback;
        return v.equalsIgnoreCase("true") || v.equals("1") || v.equalsIgnoreCase("yes");
    }

    private Main() {}
}
