package main

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// ------------------------------------------------------------- generator inference

// Generator IDs (the combobox options). "auto" is resolved to a concrete one at
// generation time; "skip"/"default" control whether the column is inserted at all.
const (
	genAuto      = "auto"
	genSkip      = "skip"
	genDefault   = "default"
	genNull      = "null"
	genSeqInt    = "seqint"
	genRandInt   = "randint"
	genRandBig   = "randbigint"
	genDecimal   = "decimal"
	genBool      = "bool"
	genUuid      = "uuid"
	genFirstName = "firstname"
	genLastName  = "lastname"
	genFullName  = "fullname"
	genUsername  = "username"
	genEmail     = "email"
	genPhone     = "phone"
	genAddress   = "address"
	genCity      = "city"
	genCountry   = "country"
	genCompany   = "company"
	genJobTitle  = "jobtitle"
	genURL       = "url"
	genIP        = "ipaddr"
	genMAC       = "macaddr"
	genDate      = "date"
	genTimestamp = "timestamp"
	genTSTime    = "ts_timestamp"
	genJSONObj   = "json_object"
	genText      = "text"
	genLorem     = "lorem"
	genConstant  = "constant"
	genEnum      = "enum"
	genFK        = "fk"
	genVector    = "pgvector"
	genTSMetric  = "ts_metric"
	genTSDevice  = "ts_device"
)

// generatorChoices returns the combobox options for a column (most-relevant first).
func generatorChoices(c dgColumn, meta dgTableMeta) []string {
	base := []string{genAuto, genSkip, genDefault, genNull, genConstant}
	if c.FK != nil {
		base = append([]string{genFK}, base...)
	}
	if c.VectorDim > 0 {
		return append([]string{genVector}, base...)
	}
	if len(c.Enum) > 0 {
		return append([]string{genEnum}, base...)
	}
	switch pgKind(c.UDT) {
	case "int":
		return append(base, genSeqInt, genRandInt, genRandBig, genUuid)
	case "float":
		return append(base, genDecimal, genRandInt, genTSMetric)
	case "bool":
		return append(base, genBool)
	case "time":
		opts := []string{genTimestamp, genDate, genTSTime}
		return append(base, opts...)
	case "uuid":
		return append([]string{genUuid}, base...)
	case "json":
		return append(base, genJSONObj, genText)
	case "net":
		return append(base, genIP, genMAC)
	default: // text-ish
		return append(base, genFirstName, genLastName, genFullName, genUsername, genEmail,
			genPhone, genCity, genCountry, genCompany, genJobTitle, genAddress, genURL,
			genText, genLorem, genTSDevice)
	}
}

// pgKind buckets a udt name into a coarse family.
func pgKind(udt string) string {
	switch strings.TrimPrefix(udt, "_") {
	case "int2", "int4", "int8", "smallint", "integer", "bigint":
		return "int"
	case "numeric", "decimal", "float4", "float8", "real", "money":
		return "float"
	case "bool", "boolean":
		return "bool"
	case "date", "time", "timetz", "timestamp", "timestamptz":
		return "time"
	case "uuid":
		return "uuid"
	case "json", "jsonb":
		return "json"
	case "inet", "cidr", "macaddr", "macaddr8":
		return "net"
	default:
		return "text"
	}
}

var reFirst = mustRe(`(?i)(^|_)(first_?name|fname|given_?name|forename)($|_)`)
var reLast = mustRe(`(?i)(^|_)(last_?name|lname|surname|family_?name)($|_)`)
var reName = mustRe(`(?i)(^|_)(full_?name|name|display_?name)($|_)`)
var reEmail = mustRe(`(?i)e[-_]?mail`)
var reUser = mustRe(`(?i)(^|_)(user_?name|login|handle)($|_)`)
var rePhone = mustRe(`(?i)(phone|mobile|contact_?number|telephone|cell)`)
var reCity = mustRe(`(?i)(^|_)city($|_)`)
var reCountry = mustRe(`(?i)(^|_)country($|_)`)
var reAddr = mustRe(`(?i)address|street`)
var reCompany = mustRe(`(?i)(company|organization|organisation|employer)`)
var reJob = mustRe(`(?i)(job_?title|position|role_?name|occupation)`)
var reURL = mustRe(`(?i)(url|website|link|homepage)`)
var reIP = mustRe(`(?i)(ip_?addr|ip_?address|ipaddr)`)
var reMoney = mustRe(`(?i)(price|amount|cost|salary|balance|total|revenue|fee)`)
var reQty = mustRe(`(?i)(quantity|qty|count|num_|_num|number_of)`)
var reCreated = mustRe(`(?i)(created|inserted|registered)_?at|_?date$`)
var reUpdated = mustRe(`(?i)(updated|modified|changed)_?at`)
var reStatus = mustRe(`(?i)(^|_)(status|state)($|_)`)
var reBool = mustRe(`(?i)(^is_|_?is_|^has_|enabled|active|flag|deleted$|_?bool$)`)
var reDevice = mustRe(`(?i)(device|sensor|series|node|host)_?id`)
var reMetric = mustRe(`(?i)(temperature|temp|cpu|memory|mem_|latency|usage|load|humidity|pressure|value|reading|metric)`)
var reVec = mustRe(`(?i)(embedding|vector|feature)`)

// inferGenerator picks the best default generator for a column ("Auto-detect").
func inferGenerator(c dgColumn, meta dgTableMeta) string {
	// Postgres-managed columns: leave to the database.
	if c.IsGenerated || c.IsIdentity || strings.Contains(c.Default, "nextval(") {
		return genDefault
	}
	if c.FK != nil {
		return genFK
	}
	if c.VectorDim > 0 {
		return genVector
	}
	if len(c.Enum) > 0 {
		return genEnum
	}
	n := c.Name
	kind := pgKind(c.UDT)
	// Time-series hypertable time column.
	if meta.IsHypertable && n == meta.TimeColumn {
		return genTSTime
	}
	switch {
	case reFirst.MatchString(n):
		return genFirstName
	case reLast.MatchString(n):
		return genLastName
	case reEmail.MatchString(n):
		return genEmail
	case reUser.MatchString(n):
		return genUsername
	case rePhone.MatchString(n):
		return genPhone
	case reCity.MatchString(n):
		return genCity
	case reCountry.MatchString(n):
		return genCountry
	case reCompany.MatchString(n):
		return genCompany
	case reJob.MatchString(n):
		return genJobTitle
	case reURL.MatchString(n):
		return genURL
	case reAddr.MatchString(n):
		return genAddress
	case reName.MatchString(n) && kind == "text":
		return genFullName
	}
	switch kind {
	case "uuid":
		return genUuid
	case "bool":
		return genBool
	case "int":
		if reQty.MatchString(n) {
			return genRandInt
		}
		if c.UDT == "int8" || c.UDT == "bigint" {
			return genRandBig
		}
		return genRandInt
	case "float":
		if reMoney.MatchString(n) {
			return genDecimal
		}
		if reMetric.MatchString(n) || reVec.MatchString(n) {
			return genTSMetric
		}
		return genDecimal
	case "time":
		if reUpdated.MatchString(n) || reCreated.MatchString(n) {
			return genTimestamp
		}
		if c.UDT == "date" {
			return genDate
		}
		return genTimestamp
	case "json":
		return genJSONObj
	case "net":
		return genIP
	default:
		if reStatus.MatchString(n) {
			return genEnum // no enum labels → falls back to a status word list
		}
		if reDevice.MatchString(n) {
			return genTSDevice
		}
		if reBool.MatchString(n) {
			return genBool
		}
		return genText
	}
}

// ------------------------------------------------------------- value generation

type genCtx struct {
	rng *rand.Rand
	fk  map[string][]string // column → sampled literals from the referenced table
	row int64               // running row index (sequential ints / timestamps)
	// time-series
	tsStart  time.Time
	tsStep   time.Duration
	deviceID string
}

// colGen is a resolved per-column generator (id + options + column meta).
type colGen struct {
	col  dgColumn
	gen  string
	opts map[string]any
}

func (g colGen) optF(k string, def float64) float64 {
	if v, ok := g.opts[k]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return def
}
func (g colGen) optS(k, def string) string {
	if v, ok := g.opts[k]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return def
}

// value returns a SQL literal for one row (or "NULL"/"DEFAULT").
func (g colGen) value(ctx *genCtx) string {
	// NULL percentage applies to any nullable column.
	if g.col.Nullable {
		if p := g.optF("nullPct", 0); p > 0 && ctx.rng.Float64()*100 < p {
			return "NULL"
		}
	}
	gen := g.gen
	if gen == genAuto {
		gen = inferGenerator(g.col, dgTableMeta{})
	}
	q := func(s string) string { return sqlLit(s) }
	r := ctx.rng
	switch gen {
	case genDefault:
		return "DEFAULT"
	case genNull:
		return "NULL"
	case genConstant:
		c := g.optS("value", "")
		if pgKind(g.col.UDT) == "int" || pgKind(g.col.UDT) == "float" || pgKind(g.col.UDT) == "bool" {
			if c == "" {
				return "0"
			}
			return c
		}
		return q(c)
	case genSeqInt:
		return strconv.FormatInt(int64(g.optF("start", 1))+ctx.row, 10)
	case genRandInt:
		lo, hi := int64(g.optF("min", 0)), int64(g.optF("max", 100000))
		return strconv.FormatInt(lo+r.Int63n(maxI(hi-lo, 1)), 10)
	case genRandBig:
		return strconv.FormatInt(r.Int63(), 10)
	case genDecimal:
		lo, hi := g.optF("min", 0), g.optF("max", 10000)
		scale := g.col.NumScale
		if scale == 0 {
			scale = 2
		}
		return strconv.FormatFloat(lo+r.Float64()*(hi-lo), 'f', scale, 64)
	case genBool:
		if r.Intn(2) == 0 {
			return "false"
		}
		return "true"
	case genUuid:
		return q(randUUID(r))
	case genFirstName:
		return clip(q(pick(r, firstNames)), g.col)
	case genLastName:
		return clip(q(pick(r, lastNames)), g.col)
	case genFullName:
		return clip(q(pick(r, firstNames)+" "+pick(r, lastNames)), g.col)
	case genUsername:
		return clip(q(strings.ToLower(pick(r, firstNames))+strconv.Itoa(r.Intn(1000))), g.col)
	case genEmail:
		return clip(q(strings.ToLower(pick(r, firstNames)+"."+pick(r, lastNames))+strconv.Itoa(r.Intn(1000))+"@"+pick(r, domains)), g.col)
	case genPhone:
		return q(fmt.Sprintf("+1-%03d-%03d-%04d", 200+r.Intn(800), r.Intn(1000), r.Intn(10000)))
	case genCity:
		return clip(q(pick(r, cities)), g.col)
	case genCountry:
		return clip(q(pick(r, countries)), g.col)
	case genCompany:
		return clip(q(pick(r, companies)), g.col)
	case genJobTitle:
		return clip(q(pick(r, jobTitles)), g.col)
	case genAddress:
		return clip(q(fmt.Sprintf("%d %s %s", 1+r.Intn(9999), pick(r, lastNames), pick(r, streets))), g.col)
	case genURL:
		return q("https://" + pick(r, domains) + "/" + pick(r, words))
	case genIP:
		return q(fmt.Sprintf("%d.%d.%d.%d", r.Intn(256), r.Intn(256), r.Intn(256), 1+r.Intn(254)))
	case genMAC:
		return q(fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", r.Intn(256), r.Intn(256), r.Intn(256), r.Intn(256), r.Intn(256), r.Intn(256)))
	case genDate:
		return q(randDate(r).Format("2006-01-02"))
	case genTimestamp:
		return q(randDate(r).Format("2006-01-02 15:04:05"))
	case genTSTime:
		t := ctx.tsStart.Add(time.Duration(ctx.row) * ctx.tsStep)
		return q(t.UTC().Format("2006-01-02 15:04:05"))
	case genTSMetric:
		lo, hi := g.optF("min", 0), g.optF("max", 100)
		v := lo + (hi-lo)*(0.5+0.5*math.Sin(float64(ctx.row)/20.0)) + r.Float64()*2
		return strconv.FormatFloat(v, 'f', 3, 64)
	case genTSDevice:
		return q(fmt.Sprintf("device-%04d", r.Intn(int(g.optF("devices", 100)))))
	case genJSONObj:
		obj := fmt.Sprintf(`{"id":%d,"name":%q,"active":%v}`, r.Intn(100000), pick(r, firstNames), r.Intn(2) == 0)
		if g.col.UDT == "jsonb" {
			return sqlLit(obj) + "::jsonb"
		}
		return sqlLit(obj) + "::json"
	case genText:
		return clip(q(pick(r, words)+" "+pick(r, words)+" "+pick(r, words)), g.col)
	case genLorem:
		return clip(q(lorem(r, 3+r.Intn(10))), g.col)
	case genEnum:
		if len(g.col.Enum) > 0 {
			return q(pick(r, g.col.Enum))
		}
		return q(pick(r, statuses))
	case genFK:
		s := ctx.fk[g.col.Name]
		if len(s) == 0 {
			return "NULL"
		}
		return s[r.Intn(len(s))]
	case genVector:
		dim := int(g.optF("dim", float64(g.col.VectorDim)))
		if dim <= 0 {
			dim = 3
		}
		lo, hi := g.optF("min", -1), g.optF("max", 1)
		var b strings.Builder
		b.WriteByte('[')
		for i := 0; i < dim; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.FormatFloat(lo+r.Float64()*(hi-lo), 'f', 6, 64))
		}
		b.WriteByte(']')
		cast := "::vector"
		if g.col.UDT == "halfvec" {
			cast = "::halfvec"
		}
		return sqlLit(b.String()) + cast
	default:
		return clip(q(pick(r, words)), g.col)
	}
}

// clip truncates a quoted string literal to the column's char length (best-effort).
func clip(lit string, c dgColumn) string {
	if c.CharLen <= 0 || len(lit) <= c.CharLen+2 {
		return lit
	}
	inner := strings.Trim(lit, "'")
	if len(inner) > c.CharLen {
		inner = inner[:c.CharLen]
	}
	return sqlLit(inner)
}

func maxI(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func pick(r *rand.Rand, s []string) string { return s[r.Intn(len(s))] }
func randDate(r *rand.Rand) time.Time {
	return time.Unix(1577836800+r.Int63n(157680000), 0) // 2020..2025
}
func randUUID(r *rand.Rand) string {
	b := make([]byte, 16)
	r.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
func lorem(r *rand.Rand, n int) string {
	w := make([]string, n)
	for i := range w {
		w[i] = pick(r, words)
	}
	return strings.Join(w, " ")
}
