package dev.dbcanvas.ledgersim.db;

import dev.dbcanvas.ledgersim.conn.ConnSpec;
import dev.dbcanvas.ledgersim.conn.Engine;
import dev.dbcanvas.ledgersim.conn.JdbcUrl;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.util.Properties;
import java.util.regex.Pattern;

/**
 * Creates the target database if it is not there yet.
 *
 * <p>A JDBC URL names a database that must already exist — PostgreSQL answers
 * {@code FATAL: database "ledgersim" does not exist} and stops, which is a
 * confusing first impression for a node whose whole job is to be pointed at a
 * fresh server. So before the pool is built, a connection is made to a
 * *bootstrap* database that always exists ({@code postgres}, or no database at
 * all on MySQL) and the real one is created there.
 *
 * <p>This is best-effort by design. If the user cannot create databases, the
 * failure is logged and the pool is built anyway: the error that then comes back
 * from the real connection is the one worth showing, and a node pointed at a
 * database that already exists must not be blocked by a privilege it does not
 * need. DBCanvas connects linked PostgreSQL targets as the superuser precisely
 * because an application is expected to make its own database.
 */
public final class Bootstrap {
    private static final Logger log = LoggerFactory.getLogger(Bootstrap.class);

    /**
     * A database name safe to interpolate into DDL. The name comes off a form,
     * and no amount of quoting makes an arbitrary string safe to concatenate —
     * so anything that is not a plain identifier is refused outright rather than
     * escaped. DBCanvas rejects the reserved system names before this as well.
     */
    private static final Pattern SAFE_NAME = Pattern.compile("[A-Za-z0-9_][A-Za-z0-9_$]{0,62}");

    private Bootstrap() {}

    public static void ensureDatabase(ConnSpec spec) {
        String name = spec.database == null ? "" : spec.database.trim();
        if (name.isEmpty()) return;
        if (!SAFE_NAME.matcher(name).matches()) {
            log.warn("database name {} is not a plain identifier; not creating it", name);
            return;
        }

        ConnSpec boot = spec.copy();
        // An override names its own database; second-guessing it would be wrong.
        if (boot.urlOverride != null && !boot.urlOverride.isBlank()) return;
        boot.database = spec.engine == Engine.POSTGRES ? "postgres" : "";

        Properties props = new Properties();
        if (spec.user != null && !spec.user.isEmpty()) props.setProperty("user", spec.user);
        if (spec.password != null && !spec.password.isEmpty()) props.setProperty("password", spec.password);

        try {
            Class.forName(spec.driver.className);
            try (Connection c = DriverManager.getConnection(JdbcUrl.build(boot), props)) {
                if (exists(c, spec.engine, name)) return;
                try (Statement st = c.createStatement()) {
                    // CREATE DATABASE cannot run inside a transaction on PostgreSQL;
                    // DriverManager hands back an autocommit connection, so it does not.
                    st.executeUpdate(spec.engine == Engine.POSTGRES
                            ? "CREATE DATABASE \"" + name + "\""
                            : "CREATE DATABASE IF NOT EXISTS `" + name + "`");
                }
                log.info("created database {}", name);
            }
        } catch (ClassNotFoundException | SQLException e) {
            // Two ordinary cases land here and neither is fatal: no privilege to
            // create databases, and another Ledger Sim node winning the race to
            // create this one.
            log.info("could not pre-create database {} ({}); connecting anyway", name, e.getMessage());
        }
    }

    private static boolean exists(Connection c, Engine engine, String name) throws SQLException {
        String sql = engine == Engine.POSTGRES
                ? "SELECT 1 FROM pg_database WHERE datname = ?"
                : "SELECT 1 FROM information_schema.schemata WHERE schema_name = ?";
        try (PreparedStatement ps = c.prepareStatement(sql)) {
            ps.setString(1, name);
            try (ResultSet rs = ps.executeQuery()) {
                return rs.next();
            }
        }
    }
}
