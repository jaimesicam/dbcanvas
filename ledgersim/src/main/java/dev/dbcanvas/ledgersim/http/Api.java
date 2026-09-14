package dev.dbcanvas.ledgersim.http;

import com.fasterxml.jackson.databind.JsonNode;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpHandler;
import com.sun.net.httpserver.HttpServer;
import dev.dbcanvas.ledgersim.LedgerApp;
import dev.dbcanvas.ledgersim.conn.ConnSpec;
import dev.dbcanvas.ledgersim.conn.DriverKind;
import dev.dbcanvas.ledgersim.conn.Engine;
import dev.dbcanvas.ledgersim.conn.PoolManager;
import dev.dbcanvas.ledgersim.conn.PoolSpec;
import dev.dbcanvas.ledgersim.sim.Knobs;
import dev.dbcanvas.ledgersim.sim.Workload;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.sql.SQLException;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.Executors;

/**
 * The HTTP surface: a handful of JSON endpoints and the static dashboard.
 *
 * <p>Built on the JDK's own {@code com.sun.net.httpserver} rather than a
 * framework — one fewer dependency to licence-audit for a server that has six
 * routes, and it keeps the shaded jar to the drivers, the pool and Jackson.
 */
public final class Api {
    private static final Logger log = LoggerFactory.getLogger(Api.class);

    private final LedgerApp app;

    public Api(LedgerApp app) { this.app = app; }

    public HttpServer start(int port) throws IOException {
        HttpServer server = HttpServer.create(new InetSocketAddress(port), 0);
        server.createContext("/healthz", this::healthz);
        server.createContext("/api/state", guarded(this::state));
        server.createContext("/api/connection/test", guarded(this::connectionTest));
        server.createContext("/api/connection/apply", guarded(this::connectionApply));
        server.createContext("/api/connection/reset", guarded(this::connectionReset));
        server.createContext("/api/workload", guarded(this::workload));
        server.createContext("/api/dataset/seed", guarded(this::seed));
        server.createContext("/", guarded(this::statik));
        server.setExecutor(Executors.newFixedThreadPool(8));
        server.start();
        log.info("dashboard listening on :{}", port);
        return server;
    }

    /**
     * Wraps a handler so an unchecked exception becomes a 500 with a body rather
     * than a closed connection with no response at all. Learned the hard way:
     * HikariCP signals a failed pool start with a RuntimeException, and the
     * dashboard's fetch() then rejected with nothing to show the user.
     */
    private HttpHandler guarded(HttpHandler inner) {
        return ex -> {
            try {
                inner.handle(ex);
            } catch (IOException e) {
                throw e;
            } catch (Exception e) {
                log.warn("unhandled error on {}: {}", ex.getRequestURI(), e.toString(), e);
                try {
                    Json.error(ex, 500, e.getClass().getSimpleName() + ": " + e.getMessage());
                } catch (IOException ignored) {
                    // The client is gone; nothing useful left to do.
                }
            } finally {
                ex.close();
            }
        };
    }

    // ------------------------------------------------------------------ routes

    private void healthz(HttpExchange ex) throws IOException {
        byte[] body = "ok\n".getBytes(StandardCharsets.UTF_8);
        ex.getResponseHeaders().set("Content-Type", "text/plain; charset=utf-8");
        ex.sendResponseHeaders(200, body.length);
        try (OutputStream os = ex.getResponseBody()) {
            os.write(body);
        }
    }

    /** Everything the dashboard polls, in one request. */
    private void state(HttpExchange ex) throws IOException {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("label", app.label());
        m.put("connection", app.connectionState());
        m.put("poolMetrics", app.pool().metrics());
        m.put("workload", workloadState());
        m.put("metrics", app.workload().metrics().snapshot());
        m.put("dataset", app.dataset());
        m.put("lastError", app.lastError());
        m.put("isolationLevels", List.of("", "TRANSACTION_READ_UNCOMMITTED", "TRANSACTION_READ_COMMITTED",
                "TRANSACTION_REPEATABLE_READ", "TRANSACTION_SERIALIZABLE"));
        Json.write(ex, 200, m);
    }

    private Map<String, Object> workloadState() {
        Knobs k = app.workload().knobs();
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("running", app.workload().running());
        m.put("threads", k.threads);
        m.put("ratePerSec", k.ratePerSec);
        m.put("isolation", k.isolation);
        m.put("linesPerOrder", k.linesPerOrder);
        m.put("customers", k.customers);
        m.put("revenueShards", k.revenueShards);
        m.put("hotAccountShare", k.hotAccountShare);
        m.put("hotAccountCount", k.hotAccountCount);
        m.put("deadlockShare", k.deadlockShare);
        m.put("lockWaitSeconds", k.lockWaitSeconds);
        m.put("maxRetries", k.maxRetries);
        m.put("weightPlace", k.weightPlace);
        m.put("weightSettle", k.weightSettle);
        m.put("weightRefund", k.weightRefund);
        m.put("weightRead", k.weightRead);
        return m;
    }

    /** Tries a candidate configuration without disturbing the live pool. */
    private void connectionTest(HttpExchange ex) throws IOException {
        if (!post(ex)) return;
        try {
            ConnSpec candidate = specFrom(Json.read(ex), app.pool().spec());
            Json.write(ex, 200, PoolManager.probe(candidate, 8_000));
        } catch (IllegalArgumentException e) {
            Json.error(ex, 400, e.getMessage());
        }
    }

    /** Rebuilds the pool against a new configuration, or explains why it cannot. */
    private void connectionApply(HttpExchange ex) throws IOException {
        if (!post(ex)) return;
        try {
            JsonNode body = Json.read(ex);
            ConnSpec spec = specFrom(body, app.pool().spec());
            PoolSpec pool = poolFrom(body.get("pool"), app.pool().poolSpec());
            app.reconfigure(spec, pool);
            Map<String, Object> out = new LinkedHashMap<>();
            out.put("ok", true);
            out.put("connection", app.connectionState());
            Json.write(ex, 200, out);
        } catch (IllegalArgumentException e) {
            Json.error(ex, 400, e.getMessage());
        } catch (SQLException e) {
            Map<String, Object> out = new LinkedHashMap<>();
            out.put("ok", false);
            out.put("error", e.getMessage());
            out.put("sqlState", e.getSQLState());
            out.put("vendorCode", e.getErrorCode());
            out.put("hint", "nothing changed — the previous connection is still live");
            Json.write(ex, 409, out);
        }
    }

    /** Back to exactly what DBCanvas detected at deploy. */
    private void connectionReset(HttpExchange ex) throws IOException {
        if (!post(ex)) return;
        try {
            app.reconfigure(app.detected(), new PoolSpec());
            Map<String, Object> out = new LinkedHashMap<>();
            out.put("ok", true);
            out.put("connection", app.connectionState());
            Json.write(ex, 200, out);
        } catch (SQLException e) {
            Json.error(ex, 409, e.getMessage());
        }
    }

    private void workload(HttpExchange ex) throws IOException {
        if (!post(ex)) return;
        JsonNode body = Json.read(ex);
        Knobs k = app.workload().knobs();
        k.threads = Json.integer(body, "threads", k.threads);
        k.ratePerSec = Json.integer(body, "ratePerSec", k.ratePerSec);
        k.isolation = Json.str(body, "isolation", k.isolation);
        k.linesPerOrder = Json.integer(body, "linesPerOrder", k.linesPerOrder);
        k.customers = Json.integer(body, "customers", k.customers);
        k.revenueShards = Json.integer(body, "revenueShards", k.revenueShards);
        k.hotAccountShare = Json.dbl(body, "hotAccountShare", k.hotAccountShare);
        k.hotAccountCount = Json.integer(body, "hotAccountCount", k.hotAccountCount);
        k.deadlockShare = Json.dbl(body, "deadlockShare", k.deadlockShare);
        k.lockWaitSeconds = Json.integer(body, "lockWaitSeconds", k.lockWaitSeconds);
        k.maxRetries = Json.integer(body, "maxRetries", k.maxRetries);
        k.weightPlace = Json.integer(body, "weightPlace", k.weightPlace);
        k.weightSettle = Json.integer(body, "weightSettle", k.weightSettle);
        k.weightRefund = Json.integer(body, "weightRefund", k.weightRefund);
        k.weightRead = Json.integer(body, "weightRead", k.weightRead);

        if (k.isolation != null && !k.isolation.isBlank() && Workload.isolationLevel(k.isolation) == null) {
            Json.error(ex, 400, "unknown isolation level: " + k.isolation);
            return;
        }
        app.workload().setKnobs(k);

        String run = Json.str(body, "run", "");
        if ("start".equals(run)) app.workload().start();
        else if ("stop".equals(run)) app.workload().stop();

        Json.write(ex, 200, Map.of("ok", true, "workload", workloadState()));
    }

    private void seed(HttpExchange ex) throws IOException {
        if (!post(ex)) return;
        try {
            Json.write(ex, 200, Map.of("ok", true, "accounts", app.seed()));
        } catch (SQLException e) {
            Json.error(ex, 500, e.getMessage());
        }
    }

    // ------------------------------------------------------------------ parsing

    /**
     * Builds a ConnSpec from a request body, falling back field by field to what
     * is currently live — so the dashboard can PATCH one field without having to
     * resend a password it was never shown.
     */
    private static ConnSpec specFrom(JsonNode body, ConnSpec current) {
        ConnSpec s = current.copy();

        String engine = Json.str(body, "engine", s.engine.id);
        Engine e = Engine.ofOrNull(engine);
        if (e == null) throw new IllegalArgumentException("unknown engine: " + engine);
        s.engine = e;

        String driver = Json.str(body, "driver", s.driver.id);
        DriverKind d = DriverKind.ofOrNull(driver);
        if (d == null) throw new IllegalArgumentException("unknown driver: " + driver);
        if (d.engine != s.engine) {
            throw new IllegalArgumentException(d.label + " cannot speak to " + s.engine.label);
        }
        s.driver = d;

        JsonNode hosts = body.get("hosts");
        if (hosts != null && hosts.isArray()) {
            List<String> list = new ArrayList<>();
            hosts.forEach(h -> {
                String t = h.asText("").trim();
                if (!t.isEmpty()) list.add(t);
            });
            s.hosts = list;
        } else if (body.has("host")) {
            s.hosts = splitHosts(Json.str(body, "host", ""));
        }

        s.port = Json.integer(body, "port", s.port);
        s.database = Json.str(body, "database", s.database);
        s.user = Json.str(body, "user", s.user);
        // An absent password keeps the current one; an explicitly empty one clears it.
        if (body.has("password")) s.password = Json.str(body, "password", "");
        s.tls = Json.str(body, "tls", s.tls);
        s.urlOverride = Json.str(body, "urlOverride", s.urlOverride);
        if (body.has("props")) s.props = new LinkedHashMap<>(Json.stringMap(body, "props"));
        return s;
    }

    private static PoolSpec poolFrom(JsonNode n, PoolSpec current) {
        PoolSpec p = current.copy();
        if (n == null || !n.isObject()) return p;
        String mode = Json.str(n, "mode", p.mode);
        if (!"pooled".equalsIgnoreCase(mode) && !"direct".equalsIgnoreCase(mode)) {
            throw new IllegalArgumentException("unknown connection mode: " + mode + " (pooled or direct)");
        }
        p.mode = mode.toLowerCase();
        p.maximumPoolSize = Json.integer(n, "maximumPoolSize", p.maximumPoolSize);
        p.minimumIdle = Json.integer(n, "minimumIdle", p.minimumIdle);
        p.connectionTimeoutMs = Json.lng(n, "connectionTimeoutMs", p.connectionTimeoutMs);
        p.idleTimeoutMs = Json.lng(n, "idleTimeoutMs", p.idleTimeoutMs);
        p.maxLifetimeMs = Json.lng(n, "maxLifetimeMs", p.maxLifetimeMs);
        p.keepaliveTimeMs = Json.lng(n, "keepaliveTimeMs", p.keepaliveTimeMs);
        p.validationTimeoutMs = Json.lng(n, "validationTimeoutMs", p.validationTimeoutMs);
        p.leakDetectionThresholdMs = Json.lng(n, "leakDetectionThresholdMs", p.leakDetectionThresholdMs);
        p.initializationFailTimeoutMs = Json.lng(n, "initializationFailTimeoutMs", p.initializationFailTimeoutMs);
        p.autoCommit = Json.bool(n, "autoCommit", p.autoCommit);
        p.readOnly = Json.bool(n, "readOnly", p.readOnly);
        p.transactionIsolation = Json.str(n, "transactionIsolation", p.transactionIsolation);
        p.connectionInitSql = Json.str(n, "connectionInitSql", p.connectionInitSql);
        p.poolName = Json.str(n, "poolName", p.poolName);
        return p;
    }

    public static List<String> splitHosts(String raw) {
        List<String> out = new ArrayList<>();
        if (raw == null) return out;
        for (String part : raw.split(",")) {
            String t = part.trim();
            if (!t.isEmpty()) out.add(t);
        }
        return out;
    }

    // ------------------------------------------------------------------ static

    private void statik(HttpExchange ex) throws IOException {
        String path = ex.getRequestURI().getPath();
        if (path == null || path.equals("/") || path.isEmpty()) path = "/index.html";
        // Nothing here is user-supplied on disk, but a traversal attempt should not
        // reach getResourceAsStream at all.
        if (path.contains("..")) {
            Json.error(ex, 400, "bad path");
            return;
        }
        String resource = "/web" + path;
        try (InputStream in = Api.class.getResourceAsStream(resource)) {
            if (in == null) {
                byte[] body = "not found\n".getBytes(StandardCharsets.UTF_8);
                ex.sendResponseHeaders(404, body.length);
                try (OutputStream os = ex.getResponseBody()) {
                    os.write(body);
                }
                return;
            }
            byte[] body = in.readAllBytes();
            ex.getResponseHeaders().set("Content-Type", contentType(path));
            ex.sendResponseHeaders(200, body.length);
            try (OutputStream os = ex.getResponseBody()) {
                os.write(body);
            }
        }
    }

    private static String contentType(String path) {
        if (path.endsWith(".html")) return "text/html; charset=utf-8";
        if (path.endsWith(".js")) return "application/javascript; charset=utf-8";
        if (path.endsWith(".css")) return "text/css; charset=utf-8";
        if (path.endsWith(".svg")) return "image/svg+xml";
        return "application/octet-stream";
    }

    private static boolean post(HttpExchange ex) throws IOException {
        if ("POST".equalsIgnoreCase(ex.getRequestMethod())) return true;
        ex.getResponseHeaders().set("Allow", "POST");
        Json.error(ex, 405, "POST only");
        return false;
    }
}
