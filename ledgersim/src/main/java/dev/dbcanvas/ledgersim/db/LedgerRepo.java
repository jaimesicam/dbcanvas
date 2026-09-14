package dev.dbcanvas.ledgersim.db;

import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ThreadLocalRandom;

/**
 * The transactions themselves. Each method is one unit of work against one
 * connection, with the caller owning the transaction boundary — the workload
 * needs to count commits, rollbacks and retries, and it cannot do that if the
 * repository quietly swallows them.
 *
 * <p>Lock ordering is the thing to read carefully here. {@link #placeOrder}
 * takes the customer row first and the revenue row second, always, which is what
 * makes it deadlock-free under concurrency. {@link #placeOrderReversed} is the
 * same transaction with the two acquisitions swapped, and exists so the workload
 * can produce real engine-detected deadlocks on demand rather than describing
 * them. They are otherwise identical on purpose: the only variable is the order.
 */
public final class LedgerRepo {
    private final Dialect d;

    public LedgerRepo(Dialect d) { this.d = d; }

    public record Line(String sku, int qty, long unitMinor) {}

    /** A placed order: what it cost and which rows it touched. */
    public record Placed(long orderId, long totalMinor, long customerId, long revenueId) {}

    public Placed placeOrder(Connection c, long customerId, int revenueShards, List<Line> lines)
            throws SQLException {
        return place(c, customerId, revenueShards, lines, false);
    }

    /** placeOrder with the two row locks acquired the wrong way round. */
    public Placed placeOrderReversed(Connection c, long customerId, int revenueShards, List<Line> lines)
            throws SQLException {
        return place(c, customerId, revenueShards, lines, true);
    }

    private Placed place(Connection c, long customerId, int revenueShards, List<Line> lines, boolean reversed)
            throws SQLException {
        long total = 0;
        for (Line l : lines) total += l.qty() * l.unitMinor();
        long revenueId = 1 + Math.floorMod(customerId, Math.max(1, revenueShards));

        if (reversed) {
            lockAccount(c, revenueId);
            lockAccount(c, customerId);
        } else {
            lockAccount(c, customerId);
            lockAccount(c, revenueId);
        }

        long orderId;
        String insertOrder = "INSERT INTO orders (account_id, status, total_minor, created_at, updated_at) "
                + "VALUES (?, 'PLACED', ?, " + d.nowExpr() + ", " + d.nowExpr() + ")";
        try (PreparedStatement ps = c.prepareStatement(insertOrder, Statement.RETURN_GENERATED_KEYS)) {
            ps.setLong(1, customerId);
            ps.setLong(2, total);
            ps.executeUpdate();
            try (ResultSet keys = ps.getGeneratedKeys()) {
                if (!keys.next()) throw new SQLException("order insert returned no generated key");
                orderId = keys.getLong(1);
            }
        }

        try (PreparedStatement ps = c.prepareStatement(
                "INSERT INTO order_lines (order_id, sku, qty, unit_minor) VALUES (?, ?, ?, ?)")) {
            for (Line l : lines) {
                ps.setLong(1, orderId);
                ps.setString(2, l.sku());
                ps.setInt(3, l.qty());
                ps.setLong(4, l.unitMinor());
                ps.addBatch();
            }
            ps.executeBatch();
        }

        adjustBalance(c, customerId, -total);
        adjustBalance(c, revenueId, total);
        post(c, orderId, customerId, 'D', total, "order");
        post(c, orderId, revenueId, 'C', total, "order");

        return new Placed(orderId, total, customerId, revenueId);
    }

    /**
     * Moves a placed order to SETTLED. Returns false when another worker got
     * there first, which is not an error — it is the normal outcome of two
     * workers racing for the same backlog and is counted separately.
     */
    public boolean settleOrder(Connection c, long orderId) throws SQLException {
        try (PreparedStatement ps = c.prepareStatement(
                "UPDATE orders SET status = 'SETTLED', updated_at = " + d.nowExpr()
                        + " WHERE id = ? AND status = 'PLACED'")) {
            ps.setLong(1, orderId);
            return ps.executeUpdate() > 0;
        }
    }

    /**
     * Reverses an order: the balances move back and two more postings are written
     * rather than the originals being deleted, because a ledger that edits history
     * is not a ledger. The balance check therefore stays at zero either way.
     */
    public boolean refundOrder(Connection c, long orderId, int revenueShards) throws SQLException {
        long customerId;
        long total;
        try (PreparedStatement ps = c.prepareStatement(
                "SELECT account_id, total_minor FROM orders WHERE id = ? AND status IN ('PLACED','SETTLED')")) {
            ps.setLong(1, orderId);
            try (ResultSet rs = ps.executeQuery()) {
                if (!rs.next()) return false;
                customerId = rs.getLong(1);
                total = rs.getLong(2);
            }
        }
        long revenueId = 1 + Math.floorMod(customerId, Math.max(1, revenueShards));
        lockAccount(c, customerId);
        lockAccount(c, revenueId);

        try (PreparedStatement ps = c.prepareStatement(
                "UPDATE orders SET status = 'REFUNDED', updated_at = " + d.nowExpr()
                        + " WHERE id = ? AND status IN ('PLACED','SETTLED')")) {
            ps.setLong(1, orderId);
            if (ps.executeUpdate() == 0) return false;
        }
        adjustBalance(c, customerId, total);
        adjustBalance(c, revenueId, -total);
        post(c, orderId, customerId, 'C', total, "refund");
        post(c, orderId, revenueId, 'D', total, "refund");
        return true;
    }

    /** An account statement — the read side, a join the optimiser has to work at. */
    public int statement(Connection c, long accountId, int limit) throws SQLException {
        String sql = "SELECT o.id, o.status, o.total_minor, e.direction, e.amount_minor, e.posted_at "
                + "FROM orders o JOIN ledger_entries e ON e.order_id = o.id "
                + "WHERE o.account_id = ? ORDER BY e.posted_at DESC, e.id DESC LIMIT ?";
        int rows = 0;
        try (PreparedStatement ps = c.prepareStatement(sql)) {
            ps.setLong(1, accountId);
            ps.setInt(2, limit);
            try (ResultSet rs = ps.executeQuery()) {
                while (rs.next()) rows++;
            }
        }
        return rows;
    }

    /** A PLACED order id to work on, or -1. Random offset so workers collide sometimes but not always. */
    public long pickOpenOrder(Connection c, int window) throws SQLException {
        String sql = "SELECT id FROM orders WHERE status = 'PLACED' ORDER BY id DESC LIMIT ?";
        List<Long> ids = new ArrayList<>();
        try (PreparedStatement ps = c.prepareStatement(sql)) {
            ps.setInt(1, Math.max(1, window));
            try (ResultSet rs = ps.executeQuery()) {
                while (rs.next()) ids.add(rs.getLong(1));
            }
        }
        if (ids.isEmpty()) return -1;
        return ids.get(ThreadLocalRandom.current().nextInt(ids.size()));
    }

    /**
     * The books, summed. Debits are negative and credits positive, so a correct
     * ledger totals exactly zero — any other number means a transaction was seen
     * half-applied, which is the point of showing it on the dashboard.
     */
    public long balanceCheck(Connection c) throws SQLException {
        try (Statement st = c.createStatement();
             ResultSet rs = st.executeQuery(
                     "SELECT COALESCE(SUM(CASE WHEN direction = 'C' THEN amount_minor "
                             + "ELSE -amount_minor END), 0) FROM ledger_entries")) {
            return rs.next() ? rs.getLong(1) : 0;
        }
    }

    public long countRows(Connection c, String table) throws SQLException {
        // The table name is never user input — callers pass a literal from this file.
        try (Statement st = c.createStatement();
             ResultSet rs = st.executeQuery("SELECT COUNT(*) FROM " + table)) {
            return rs.next() ? rs.getLong(1) : 0;
        }
    }

    private void lockAccount(Connection c, long id) throws SQLException {
        try (PreparedStatement ps = c.prepareStatement(
                "SELECT balance_minor FROM accounts WHERE id = ? FOR UPDATE")) {
            ps.setLong(1, id);
            try (ResultSet rs = ps.executeQuery()) {
                if (!rs.next()) throw new SQLException("no such account: " + id);
            }
        }
    }

    private void adjustBalance(Connection c, long id, long delta) throws SQLException {
        try (PreparedStatement ps = c.prepareStatement(
                "UPDATE accounts SET balance_minor = balance_minor + ?, version = version + 1, "
                        + "updated_at = " + d.nowExpr() + " WHERE id = ?")) {
            ps.setLong(1, delta);
            ps.setLong(2, id);
            ps.executeUpdate();
        }
    }

    private void post(Connection c, long orderId, long accountId, char direction, long amount, String memo)
            throws SQLException {
        try (PreparedStatement ps = c.prepareStatement(
                "INSERT INTO ledger_entries (order_id, account_id, direction, amount_minor, memo, posted_at) "
                        + "VALUES (?, ?, ?, ?, ?, " + d.nowExpr() + ")")) {
            ps.setLong(1, orderId);
            ps.setLong(2, accountId);
            ps.setString(3, String.valueOf(direction));
            ps.setLong(4, amount);
            ps.setString(5, memo);
            ps.executeUpdate();
        }
    }
}
