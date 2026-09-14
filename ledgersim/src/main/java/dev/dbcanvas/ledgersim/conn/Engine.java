package dev.dbcanvas.ledgersim.conn;

/**
 * The relational engines Ledger Sim speaks. JDBC is the whole point of this
 * simulator, so the list is shorter than the Stock Market Sim's on purpose:
 * MongoDB and Valkey have no JDBC driver worth the name, and pretending
 * otherwise with a wrapper would teach the wrong thing about both.
 */
public enum Engine {
    MYSQL("mysql", "MySQL / Percona Server / PXC", 3306),
    POSTGRES("postgres", "PostgreSQL", 5432);

    public final String id;
    public final String label;
    public final int defaultPort;

    Engine(String id, String label, int defaultPort) {
        this.id = id;
        this.label = label;
        this.defaultPort = defaultPort;
    }

    public static Engine of(String id) {
        for (Engine e : values()) if (e.id.equalsIgnoreCase(id)) return e;
        throw new IllegalArgumentException("unknown engine: " + id);
    }

    public static Engine ofOrNull(String id) {
        for (Engine e : values()) if (e.id.equalsIgnoreCase(id)) return e;
        return null;
    }
}
