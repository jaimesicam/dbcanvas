package search

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// Shell renders a pipeline as the mongosh command that runs it:
//
//	db.tickets.aggregate([
//	  { $vectorSearch: { index: "tickets_vector", queryVector: [0.0213, -0.0441, … 384 numbers], … } },
//	  …
//	])
//
// Keys are unquoted the way people write them, dates are ISODate(...), and a
// query vector is shown as its first few numbers and a count — 384 numbers on
// screen teach nothing that four and "384" do not.
func Shell(coll string, p bson.A) string {
	var b strings.Builder
	fmt.Fprintf(&b, "db.%s.aggregate([\n", coll)
	for i, st := range p {
		b.WriteString("  ")
		render(&b, st, 1)
		if i < len(p)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("])")
	return b.String()
}

func render(b *strings.Builder, v any, depth int) {
	switch x := v.(type) {
	case bson.D:
		if len(x) == 0 {
			b.WriteString("{}")
			return
		}
		// Short documents stay on one line; the stage bodies of $vectorSearch and
		// $rankFusion break across lines so each option can be read.
		if inline(x) {
			b.WriteString("{ ")
			for i, e := range x {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(key(e.Key) + ": ")
				render(b, e.Value, depth+1)
			}
			b.WriteString(" }")
			return
		}
		pad := strings.Repeat("  ", depth+1)
		b.WriteString("{\n")
		for i, e := range x {
			b.WriteString(pad + key(e.Key) + ": ")
			render(b, e.Value, depth+1)
			if i < len(x)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(strings.Repeat("  ", depth) + "}")
	case bson.A:
		if len(x) == 1 {
			if _, ok := x[0].(bson.D); ok && !inline(x[0].(bson.D)) {
				b.WriteString("[")
				render(b, x[0], depth)
				b.WriteString("]")
				return
			}
		}
		b.WriteString("[")
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			render(b, e, depth+1)
		}
		b.WriteString("]")
	case []string:
		a := make(bson.A, len(x))
		for i, s := range x {
			a[i] = s
		}
		render(b, a, depth)
	case []float32:
		b.WriteString("[")
		n := 4
		if len(x) < n {
			n = len(x)
		}
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strconv.FormatFloat(float64(x[i]), 'f', 4, 32))
		}
		if len(x) > n {
			fmt.Fprintf(b, ", … %d numbers", len(x))
		}
		b.WriteString("]")
	case string:
		b.WriteString(strconv.Quote(x))
	case time.Time:
		b.WriteString(`ISODate("` + x.UTC().Format(time.RFC3339) + `")`)
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int32, int64:
		fmt.Fprintf(b, "%d", x)
	case float64:
		b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case nil:
		b.WriteString("null")
	default:
		fmt.Fprintf(b, "%v", x)
	}
}

// inline reports whether a document is short enough for one line.
func inline(d bson.D) bool {
	if len(d) > 3 {
		return false
	}
	for _, e := range d {
		switch v := e.Value.(type) {
		case []float32:
			return false
		case bson.D:
			if !inline(v) {
				return false
			}
		case bson.A:
			for _, x := range v {
				if dd, ok := x.(bson.D); ok && !inline(dd) {
					return false
				}
			}
		}
	}
	return true
}

func key(k string) string {
	for _, r := range k {
		if !(r == '_' || r == '$' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return strconv.Quote(k)
		}
	}
	return k
}
