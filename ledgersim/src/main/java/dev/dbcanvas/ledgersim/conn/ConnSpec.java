package dev.dbcanvas.ledgersim.conn;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Everything needed to build a JDBC URL, in the shape a person thinks about it
 * rather than the shape the URL is written in.
 *
 * <p>This is the "detected" half of the feature. DBCanvas resolves the node's
 * target at deploy time — a linked canvas node, an All-in-One instance, or a
 * manually typed host — and hands the result in as environment. That becomes a
 * ConnSpec, which renders to a URL. The user then edits any field, or the URL
 * itself, and the pool is rebuilt: see PoolManager.
 *
 * <p>{@link #urlOverride} beats every other field. It is the escape hatch for a
 * URL shape this class does not model, and the reason the UI always shows the
 * effective URL rather than assuming its own fields produced it.
 */
public final class ConnSpec {
    public Engine engine = Engine.MYSQL;
    public DriverKind driver = DriverKind.MYSQL_CONNECTOR_J;

    /** One entry per host. An entry may carry its own ":port"; otherwise {@link #port} applies. */
    public List<String> hosts = new ArrayList<>();
    public int port;
    public String database = "ledgersim";
    public String user = "";
    public String password = "";

    /** "disable" | "prefer" | "require" — mapped per driver by {@link JdbcUrl}. */
    public String tls = "prefer";

    /** Driver properties. Merged over the auto-derived ones, so a user entry always wins. */
    public Map<String, String> props = new LinkedHashMap<>();

    /** Used verbatim when non-blank; every other field is then advisory. */
    public String urlOverride = "";

    public ConnSpec copy() {
        ConnSpec c = new ConnSpec();
        c.engine = engine;
        c.driver = driver;
        c.hosts = new ArrayList<>(hosts);
        c.port = port;
        c.database = database;
        c.user = user;
        c.password = password;
        c.tls = tls;
        c.props = new LinkedHashMap<>(props);
        c.urlOverride = urlOverride;
        return c;
    }

    public int effectivePort() {
        return port > 0 ? port : engine.defaultPort;
    }

    /** Hosts with a port attached to each, which is the form every driver's URL wants. */
    public List<String> hostPorts() {
        List<String> out = new ArrayList<>();
        for (String h : hosts) {
            String t = h == null ? "" : h.trim();
            if (t.isEmpty()) continue;
            // IPv6 literals arrive bracketed; only an unbracketed ':' is a port.
            boolean hasPort = t.startsWith("[") ? t.lastIndexOf(':') > t.lastIndexOf(']') : t.contains(":");
            out.add(hasPort ? t : t + ":" + effectivePort());
        }
        return out;
    }

    public String jdbcUrl() {
        return JdbcUrl.build(this);
    }
}
