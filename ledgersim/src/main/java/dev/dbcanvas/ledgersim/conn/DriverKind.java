package dev.dbcanvas.ledgersim.conn;

import java.util.ArrayList;
import java.util.List;

/**
 * A JDBC driver the image ships, and the facts that differ between them.
 *
 * <p>Two drivers for the MySQL family is not redundancy — it is the reason this
 * simulator exists. A customer reporting "it works from the CLI but not from the
 * app" is very often reporting a driver behaviour (TLS defaults, authentication
 * plugin support, failover semantics, how a URL is parsed), and the only way to
 * separate driver from server is to point two drivers at one server and watch
 * them disagree. So the driver is a per-node choice and is swappable at runtime
 * without redeploying.
 *
 * <p>The licence on each is recorded here as well as in NOTICE because it is the
 * kind of fact that has to survive someone adding a fourth driver in a hurry:
 * DBCanvas is GPL-3.0-only and not every JDBC driver can legally be in this jar.
 */
public enum DriverKind {
    MYSQL_CONNECTOR_J(
            "mysql-connector-j", "MySQL Connector/J", "com.mysql.cj.jdbc.Driver",
            "jdbc:mysql", Engine.MYSQL, "GPL-2.0-only WITH Universal-FOSS-Exception-1.0"),
    MARIADB_CONNECTOR_J(
            "mariadb-connector-j", "MariaDB Connector/J", "org.mariadb.jdbc.Driver",
            "jdbc:mariadb", Engine.MYSQL, "LGPL-2.1-or-later"),
    PGJDBC(
            "pgjdbc", "pgJDBC", "org.postgresql.Driver",
            "jdbc:postgresql", Engine.POSTGRES, "BSD-2-Clause");

    public final String id;
    public final String label;
    public final String className;
    public final String urlScheme;
    public final Engine engine;
    public final String license;

    DriverKind(String id, String label, String className, String urlScheme, Engine engine, String license) {
        this.id = id;
        this.label = label;
        this.className = className;
        this.urlScheme = urlScheme;
        this.engine = engine;
        this.license = license;
    }

    public static DriverKind of(String id) {
        DriverKind d = ofOrNull(id);
        if (d == null) throw new IllegalArgumentException("unknown driver: " + id);
        return d;
    }

    public static DriverKind ofOrNull(String id) {
        if (id == null) return null;
        for (DriverKind d : values()) if (d.id.equalsIgnoreCase(id)) return d;
        return null;
    }

    /** The drivers that can speak to a given engine, in preference order. */
    public static List<DriverKind> forEngine(Engine e) {
        List<DriverKind> out = new ArrayList<>();
        for (DriverKind d : values()) if (d.engine == e) out.add(d);
        return out;
    }

    /** The default driver for an engine — the one a user would be handed by a vendor. */
    public static DriverKind defaultFor(Engine e) {
        return e == Engine.POSTGRES ? PGJDBC : MYSQL_CONNECTOR_J;
    }

    /** Whether the driver class is actually on the classpath (it always should be). */
    public boolean available() {
        try {
            Class.forName(className);
            return true;
        } catch (ClassNotFoundException e) {
            return false;
        }
    }
}
