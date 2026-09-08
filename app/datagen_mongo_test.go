package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func typeName(v any) string { return fmt.Sprintf("%T", v) }

func mongoTestCtx() *genCtx {
	return &genCtx{rng: newRand(1), engine: "mongodb", uniq: "test",
		tsStart: time.Now().Add(-time.Hour), tsStep: time.Minute}
}

func rawDoc(t *testing.T, d bson.D) bson.Raw {
	t.Helper()
	b, err := bson.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bson.Raw(b)
}

func colByName(cols []dgColumn, name string) (dgColumn, bool) {
	for _, c := range cols {
		if c.Name == name {
			return c, true
		}
	}
	return dgColumn{}, false
}

// Sampling a collection must recover each field's BSON type, recurse into embedded documents and
// arrays, and flag a field that is absent from some documents as nullable.
func TestMongoInfersBSONSchema(t *testing.T) {
	docs := []bson.Raw{
		rawDoc(t, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()},
			{Key: "name", Value: "Alice"},
			{Key: "age", Value: int32(30)},
			{Key: "score", Value: 9.5},
			{Key: "active", Value: true},
			{Key: "tags", Value: bson.A{"a", "b"}},
			{Key: "addr", Value: bson.D{{Key: "city", Value: "NYC"}}},
			{Key: "note", Value: "present"},
		}),
		rawDoc(t, bson.D{
			{Key: "_id", Value: primitive.NewObjectID()},
			{Key: "name", Value: "Bob"},
			{Key: "age", Value: int32(25)},
			{Key: "score", Value: 8.0},
			{Key: "active", Value: false},
			{Key: "tags", Value: bson.A{"c"}},
			{Key: "addr", Value: bson.D{{Key: "city", Value: "LA"}}},
			// "note" absent → nullable
		}),
	}
	cols := inferDocFields(docs, len(docs))
	for i := range cols {
		fillMongoGen(&cols[i])
	}

	want := map[string]string{
		"_id": "objectId", "name": "string", "age": "int", "score": "double",
		"active": "bool", "tags": "array", "addr": "object", "note": "string",
	}
	for name, udt := range want {
		c, ok := colByName(cols, name)
		if !ok {
			t.Fatalf("field %q not inferred", name)
		}
		if c.UDT != udt {
			t.Errorf("field %q: got BSON type %q, want %q", name, c.UDT, udt)
		}
	}

	// Nesting.
	if addr, _ := colByName(cols, "addr"); len(addr.Fields) != 1 || addr.Fields[0].Name != "city" || addr.Fields[0].UDT != "string" {
		t.Errorf("addr sub-schema wrong: %+v", addr.Fields)
	}
	if tags, _ := colByName(cols, "tags"); tags.Elem == nil || tags.Elem.UDT != "string" {
		t.Errorf("tags element schema wrong: %+v", tags.Elem)
	}
	// Nullability.
	if note, _ := colByName(cols, "note"); !note.Nullable {
		t.Error("note appears in only one doc; it must be nullable")
	}
	if name, _ := colByName(cols, "name"); name.Nullable {
		t.Error("name appears in every doc; it must not be nullable")
	}
}

// The default generator must match the BSON type, and string fields must pick a semantic
// generator from their name.
func TestMongoInferGenerator(t *testing.T) {
	cases := []struct {
		name, udt, want string
	}{
		{"_id", "objectId", genObjectID},
		{"email", "string", genEmail},
		{"first_name", "string", genFirstName},
		{"count", "long", genInt64},
		{"price", "decimal", genDecimal128},
		{"created", "date", genBSONDate},
		{"user_uuid", "binData", genBSONUUID},
		{"blob", "binData", genBinary},
		{"profile", "object", genEmbedded},
		{"items", "array", genArray},
		{"ts", "timestamp", genBSONTS},
		{"flag", "bool", genBool},
	}
	for _, tc := range cases {
		got := mongoInferGenerator(dgColumn{Name: tc.name, UDT: tc.udt})
		if got != tc.want {
			t.Errorf("%s (%s): got %q, want %q", tc.name, tc.udt, got, tc.want)
		}
	}
}

// Every generator must emit the correct native driver type — this is what "all BSON types" means
// concretely.
func TestMongoValueNativeTypes(t *testing.T) {
	gc := mongoTestCtx()
	check := func(gen string, c dgColumn, want any) {
		v, inc := mongoValue(c, gen, nil, gc)
		if !inc {
			t.Errorf("%s: field unexpectedly omitted", gen)
			return
		}
		gotT := typeName(v)
		wantT := typeName(want)
		if gotT != wantT {
			t.Errorf("%s: got %s, want %s", gen, gotT, wantT)
		}
	}
	check(genObjectID, dgColumn{UDT: "objectId"}, primitive.ObjectID{})
	check(genRandInt, dgColumn{UDT: "int"}, int32(0))
	check(genSeqInt, dgColumn{UDT: "int"}, int32(0))
	check(genInt64, dgColumn{UDT: "long"}, int64(0))
	check(genDouble, dgColumn{UDT: "double"}, float64(0))
	check(genDecimal128, dgColumn{UDT: "decimal"}, primitive.Decimal128{})
	check(genBool, dgColumn{UDT: "bool"}, true)
	check(genBSONDate, dgColumn{UDT: "date"}, primitive.DateTime(0))
	check(genBSONTS, dgColumn{UDT: "timestamp"}, primitive.Timestamp{})
	check(genBinary, dgColumn{UDT: "binData"}, primitive.Binary{})
	check(genBSONUUID, dgColumn{UDT: "binData"}, primitive.Binary{})
	check(genRegex, dgColumn{UDT: "regex"}, primitive.Regex{})
	check(genJavaScript, dgColumn{UDT: "javascript"}, primitive.JavaScript(""))
	check(genMinKey, dgColumn{UDT: "minKey"}, primitive.MinKey{})
	check(genMaxKey, dgColumn{UDT: "maxKey"}, primitive.MaxKey{})
	check(genEmail, dgColumn{UDT: "string"}, "")

	// UUID binary carries subtype 4; generic binary carries subtype 0.
	if v, _ := mongoValue(dgColumn{UDT: "binData"}, genBSONUUID, nil, gc); v.(primitive.Binary).Subtype != 0x04 {
		t.Error("genBSONUUID must produce binary subtype 0x04")
	}
	if v, _ := mongoValue(dgColumn{UDT: "binData"}, genBinary, nil, gc); v.(primitive.Binary).Subtype != 0x00 {
		t.Error("genBinary must produce binary subtype 0x00")
	}
}

// genSkip omits the field entirely; genNull includes it with a BSON null. These are different.
func TestMongoSkipVsNull(t *testing.T) {
	gc := mongoTestCtx()
	if _, inc := mongoValue(dgColumn{Name: "x", UDT: "string", Nullable: true}, genSkip, nil, gc); inc {
		t.Error("genSkip must omit the field")
	}
	v, inc := mongoValue(dgColumn{Name: "x", UDT: "string", Nullable: true}, genNull, nil, gc)
	if !inc || v != nil {
		t.Errorf("genNull must include the field with a nil (BSON null) value; got inc=%v v=%v", inc, v)
	}
}

// A document covering every BSON type — including nested — must marshal to Extended JSON, and the
// type-specific EJSON markers must appear. This is the end-to-end "supports all BSON types" gate.
func TestMongoDocMarshalsAllBSONTypes(t *testing.T) {
	gc := mongoTestCtx()
	embedded := dgColumn{Name: "addr", UDT: "object", Generator: genEmbedded,
		Fields: []dgColumn{{Name: "city", UDT: "string", Generator: genCity}}}
	arr := dgColumn{Name: "scores", UDT: "array", Generator: genArray,
		Elem: &dgColumn{UDT: "double", Generator: genDouble}}
	gens := []colGen{
		{col: dgColumn{Name: "_id", UDT: "objectId"}, gen: genObjectID},
		{col: dgColumn{Name: "s", UDT: "string"}, gen: genText},
		{col: dgColumn{Name: "i", UDT: "int"}, gen: genRandInt},
		{col: dgColumn{Name: "l", UDT: "long"}, gen: genInt64},
		{col: dgColumn{Name: "d", UDT: "double"}, gen: genDouble},
		{col: dgColumn{Name: "dec", UDT: "decimal"}, gen: genDecimal128},
		{col: dgColumn{Name: "b", UDT: "bool"}, gen: genBool},
		{col: dgColumn{Name: "dt", UDT: "date"}, gen: genBSONDate},
		{col: dgColumn{Name: "ts", UDT: "timestamp"}, gen: genBSONTS},
		{col: dgColumn{Name: "bin", UDT: "binData"}, gen: genBinary},
		{col: dgColumn{Name: "uuid", UDT: "binData"}, gen: genBSONUUID},
		{col: dgColumn{Name: "rx", UDT: "regex"}, gen: genRegex},
		{col: dgColumn{Name: "js", UDT: "javascript"}, gen: genJavaScript},
		{col: dgColumn{Name: "mn", UDT: "minKey"}, gen: genMinKey},
		{col: dgColumn{Name: "mx", UDT: "maxKey"}, gen: genMaxKey},
		{col: embedded, gen: genEmbedded},
		{col: arr, gen: genArray},
	}
	doc := mongoDoc(gens, gc)
	b, err := bson.MarshalExtJSON(doc, false, false)
	if err != nil {
		t.Fatalf("MarshalExtJSON: %v", err)
	}
	js := string(b)
	for _, marker := range []string{"$oid", "$numberDecimal", "$date", "$timestamp", "$binary", "$minKey", "$maxKey"} {
		if !strings.Contains(js, marker) {
			t.Errorf("EJSON missing %q marker: %s", marker, js)
		}
	}
	// Round-trips back to BSON.
	var back bson.D
	if err := bson.UnmarshalExtJSON(b, false, &back); err != nil {
		t.Fatalf("UnmarshalExtJSON: %v", err)
	}
}
