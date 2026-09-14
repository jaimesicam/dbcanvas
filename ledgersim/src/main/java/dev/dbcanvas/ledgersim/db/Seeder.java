package dev.dbcanvas.ledgersim.db;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.SQLException;

/**
 * Puts the chart of accounts in place: the revenue shards, then the customers.
 *
 * <p>Idempotent, because a Ledger Sim node is redeployed against a database that
 * may already hold its tables and the alternative — dropping and recreating — would
 * throw away the very dataset someone spent an afternoon growing. If the account
 * count already matches, seeding is a no-op and the existing orders and postings
 * are left exactly where they are.
 */
public final class Seeder {
    private static final Logger log = LoggerFactory.getLogger(Seeder.class);

    private Seeder() {}

    /** Opening balance for every customer, large enough that the workload never runs one dry. */
    private static final long OPENING_BALANCE_MINOR = 100_000_000L;

    public static long seed(Connection c, Dialect d, int customers) throws SQLException {
        LedgerRepo repo = new LedgerRepo(d);
        long existing = repo.countRows(c, "accounts");
        long wanted = Schema.REVENUE_SHARDS + (long) customers;
        if (existing >= wanted) {
            log.info("accounts already seeded ({} rows), leaving the dataset alone", existing);
            return existing;
        }

        boolean auto = c.getAutoCommit();
        c.setAutoCommit(false);
        try {
            String sql = "INSERT INTO accounts (id, name, currency, balance_minor, version, updated_at) "
                    + "VALUES (?, ?, 'USD', ?, 0, " + d.nowExpr() + ")";
            try (PreparedStatement ps = c.prepareStatement(sql)) {
                for (int i = 0; i < Schema.REVENUE_SHARDS; i++) {
                    long id = 1 + i;
                    if (id <= existing) continue;
                    ps.setLong(1, id);
                    ps.setString(2, "revenue-" + i);
                    ps.setLong(3, 0);
                    ps.addBatch();
                }
                for (int i = 0; i < customers; i++) {
                    long id = Schema.FIRST_CUSTOMER_ID + i;
                    ps.setLong(1, id);
                    ps.setString(2, "customer-" + i);
                    ps.setLong(3, OPENING_BALANCE_MINOR);
                    ps.addBatch();
                    if (i % 1000 == 999) ps.executeBatch();
                }
                ps.executeBatch();
            }
            c.commit();
        } catch (SQLException e) {
            c.rollback();
            throw e;
        } finally {
            c.setAutoCommit(auto);
        }
        log.info("seeded {} revenue shards + {} customers", Schema.REVENUE_SHARDS, customers);
        return repo.countRows(c, "accounts");
    }
}
