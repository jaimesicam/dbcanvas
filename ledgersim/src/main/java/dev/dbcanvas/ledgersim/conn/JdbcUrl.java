package dev.dbcanvas.ledgersim.conn;

import java.io.UnsupportedEncodingException;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Composes a driver-correct JDBC URL from a {@link ConnSpec}, and explains what
 * it did.
 *
 * <p>The explaining is half the value. "Connect over TLS" is one intent with
 * three different spellings and, between the two MySQL drivers, three different
 * sets of available meanings — and getting that wrong is a support case, not a
 * typo. So {@link #autoProps} derives the properties and {@link #notes} states
 * every non-obvious decision in words, which the UI renders next to the URL. A
 * user who disagrees overrides the property; a user who disagrees with the whole
 * shape overrides the URL.
 */
public final class JdbcUrl {
    private JdbcUrl() {}

    /** The effective URL: the override when set, otherwise composed from the fields. */
    public static String build(ConnSpec s) {
        if (s.urlOverride != null && !s.urlOverride.isBlank()) return s.urlOverride.trim();

        List<String> hostPorts = s.hostPorts();
        String authority = hostPorts.isEmpty() ? "" : String.join(",", hostPorts);

        StringBuilder url = new StringBuilder(s.driver.urlScheme).append("://").append(authority);
        url.append('/').append(s.database == null ? "" : s.database.trim());

        Map<String, String> props = effectiveProps(s);
        if (!props.isEmpty()) {
            StringBuilder q = new StringBuilder();
            for (Map.Entry<String, String> e : props.entrySet()) {
                if (q.length() > 0) q.append('&');
                q.append(enc(e.getKey())).append('=').append(enc(e.getValue()));
            }
            url.append('?').append(q);
        }
        return url.toString();
    }

    /** Auto-derived properties with the user's own merged over the top. */
    public static Map<String, String> effectiveProps(ConnSpec s) {
        Map<String, String> props = new LinkedHashMap<>(autoProps(s));
        if (s.props != null) {
            for (Map.Entry<String, String> e : s.props.entrySet()) {
                String k = e.getKey() == null ? "" : e.getKey().trim();
                if (k.isEmpty()) continue;
                String v = e.getValue() == null ? "" : e.getValue();
                // An explicitly empty value removes an auto-derived property rather
                // than emitting "key=" — the only way to say "leave this at the
                // driver's own default" without editing the whole URL.
                if (v.isEmpty()) props.remove(k);
                else props.put(k, v);
            }
        }
        return props;
    }

    /**
     * What DBCanvas would set if the user said nothing. Kept deliberately small:
     * every entry here is something the lab genuinely needs, because anything
     * else would be this simulator quietly not being a default installation.
     */
    public static Map<String, String> autoProps(ConnSpec s) {
        Map<String, String> p = new LinkedHashMap<>();
        String tls = s.tls == null ? "prefer" : s.tls.trim().toLowerCase();
        boolean multi = s.hostPorts().size() > 1;

        switch (s.driver) {
            case MYSQL_CONNECTOR_J -> {
                p.put("sslMode", switch (tls) {
                    case "disable" -> "DISABLED";
                    case "require" -> "REQUIRED";
                    default -> "PREFERRED";
                });
                // caching_sha2_password (the MySQL 8+ default) will not hand over
                // its RSA public key on an unencrypted connection unless asked, and
                // a first connection as a new user then fails with "Public Key
                // Retrieval is not allowed" — which reads like an auth failure.
                if (!"require".equals(tls)) p.put("allowPublicKeyRetrieval", "true");
                p.put("connectionTimeZone", "SERVER");
                p.put("characterEncoding", "UTF-8");
            }
            case MARIADB_CONNECTOR_J -> {
                // MariaDB Connector/J has no "prefer": sslMode is disable | trust |
                // verify-ca | verify-full. "trust" encrypts without validating the
                // certificate, which is what a self-signed lab CA needs; there is no
                // opportunistic mode, so "prefer" cannot be honoured and is mapped
                // to disable rather than silently forcing TLS on. See notes().
                p.put("sslMode", switch (tls) {
                    case "require" -> "trust";
                    default -> "disable";
                });
                p.put("connectionTimeZone", "SERVER");
            }
            case PGJDBC -> {
                p.put("sslmode", switch (tls) {
                    case "disable" -> "disable";
                    case "require" -> "require";
                    default -> "prefer";
                });
                p.put("ApplicationName", "ledgersim");
                // With more than one host pgjdbc will otherwise happily settle on a
                // standby and every write will fail read-only.
                if (multi) p.put("targetServerType", "primary");
            }
        }
        return p;
    }

    /** Plain-language notes on anything derived that is not a 1:1 translation. */
    public static List<String> notes(ConnSpec s) {
        List<String> out = new ArrayList<>();
        String tls = s.tls == null ? "prefer" : s.tls.trim().toLowerCase();
        boolean multi = s.hostPorts().size() > 1;

        if (s.urlOverride != null && !s.urlOverride.isBlank()) {
            out.add("The URL is overridden, so the fields above are not what is being used — "
                    + "clear the override to go back to composing it.");
            return out;
        }
        if (s.driver == DriverKind.MARIADB_CONNECTOR_J && "prefer".equals(tls)) {
            out.add("MariaDB Connector/J has no opportunistic TLS mode, so tls=prefer became "
                    + "sslMode=disable. Set tls=require (sslMode=trust) to encrypt against the lab CA.");
        }
        if (s.driver == DriverKind.MYSQL_CONNECTOR_J && !"require".equals(tls)) {
            out.add("allowPublicKeyRetrieval=true is set so caching_sha2_password can authenticate "
                    + "over an unencrypted connection. Drop it to see the failure a customer reports.");
        }
        if (s.driver == DriverKind.PGJDBC && multi) {
            out.add("targetServerType=primary is set because several hosts were given — without it "
                    + "pgjdbc may pick a standby and every write fails as read-only.");
        }
        if (multi && s.driver == DriverKind.MYSQL_CONNECTOR_J) {
            out.add("Several hosts with Connector/J means its failover mode: the first host is "
                    + "primary and the rest are tried in order after a connection error.");
        }
        if (s.hostPorts().isEmpty()) out.add("No host is set, so there is nothing to connect to yet.");
        return out;
    }

    private static String enc(String v) {
        try {
            return URLEncoder.encode(v, StandardCharsets.UTF_8.name());
        } catch (UnsupportedEncodingException e) {
            throw new IllegalStateException(e); // UTF-8 is always present
        }
    }
}
