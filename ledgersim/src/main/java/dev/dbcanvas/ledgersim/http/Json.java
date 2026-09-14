package dev.dbcanvas.ledgersim.http;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.HttpExchange;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.Map;

/** Request/response plumbing. Jackson (Apache-2.0) does the parsing. */
public final class Json {
    private static final ObjectMapper MAPPER = new ObjectMapper();

    private Json() {}

    public static void write(HttpExchange ex, int status, Object body) throws IOException {
        byte[] out = MAPPER.writeValueAsBytes(body);
        ex.getResponseHeaders().set("Content-Type", "application/json; charset=utf-8");
        ex.getResponseHeaders().set("Cache-Control", "no-store");
        ex.sendResponseHeaders(status, out.length);
        try (OutputStream os = ex.getResponseBody()) {
            os.write(out);
        }
    }

    public static void error(HttpExchange ex, int status, String message) throws IOException {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("ok", false);
        m.put("error", message);
        write(ex, status, m);
    }

    public static JsonNode read(HttpExchange ex) throws IOException {
        try (InputStream in = ex.getRequestBody()) {
            byte[] raw = in.readAllBytes();
            if (raw.length == 0) return MAPPER.createObjectNode();
            return MAPPER.readTree(new String(raw, StandardCharsets.UTF_8));
        }
    }

    public static String str(JsonNode n, String field, String fallback) {
        JsonNode v = n.get(field);
        return v == null || v.isNull() ? fallback : v.asText();
    }

    public static int integer(JsonNode n, String field, int fallback) {
        JsonNode v = n.get(field);
        return v == null || v.isNull() || !v.isNumber() && !v.canConvertToInt() ? fallback : v.asInt(fallback);
    }

    public static long lng(JsonNode n, String field, long fallback) {
        JsonNode v = n.get(field);
        return v == null || v.isNull() ? fallback : v.asLong(fallback);
    }

    public static double dbl(JsonNode n, String field, double fallback) {
        JsonNode v = n.get(field);
        return v == null || v.isNull() ? fallback : v.asDouble(fallback);
    }

    public static boolean bool(JsonNode n, String field, boolean fallback) {
        JsonNode v = n.get(field);
        return v == null || v.isNull() ? fallback : v.asBoolean(fallback);
    }

    /** A JSON object of string values → an ordered map, for the driver property table. */
    public static Map<String, String> stringMap(JsonNode n, String field) {
        Map<String, String> out = new LinkedHashMap<>();
        JsonNode v = n.get(field);
        if (v == null || !v.isObject()) return out;
        v.fields().forEachRemaining(e -> out.put(e.getKey(), e.getValue().asText("")));
        return out;
    }
}
