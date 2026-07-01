package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
)

// MySQL / PXC introspection for the Data Generator. All SQL runs via the mysql client
// (see queryJSON/execSQL in datagen.go). In MySQL a "schema" is a database, so the schema
// passed from the UI equals the selected database.

// myCol mirrors dgColumn plus an ordinal (MySQL's JSON_ARRAYAGG isn't ordered). MySQL 8's
// JSON_OBJECT emits comparison/logical expressions as JSON true/false, so bool fields work.
type myCol struct {
	Ord          int    `json:"ord"`
	Name         string `json:"name"`
	DataType     string `json:"dataType"`
	UDT          string `json:"udt"`
	Nullable     bool   `json:"nullable"`
	Default      string `json:"default"`
	IsIdentity   bool   `json:"isIdentity"`
	IsGenerated  bool   `json:"isGenerated"`
	IsPrimaryKey bool   `json:"isPrimaryKey"`
	IsUnique     bool   `json:"isUnique"`
	CharLen      int    `json:"charLen"`
	NumPrecision int    `json:"numPrecision"`
	NumScale     int    `json:"numScale"`
}

// myTableMeta introspects a MySQL/PXC table's columns + PK/unique + single-column FKs and
// fills each column's inferred generator + choices.
func (a *App) myTableMeta(ctx context.Context, c dbConn, db, schema, table string) (dgTableMeta, error) {
	if schema == "" {
		schema = db
	}
	if !identOK(schema) || !identOK(table) {
		return dgTableMeta{}, fmt.Errorf("invalid schema/table")
	}
	meta := dgTableMeta{Database: db, Schema: schema, Table: table}

	// Columns (JSON_ARRAYAGG isn't ordered — sort by ordinal in Go).
	colQ := fmt.Sprintf(`SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT(
	  'ord', ordinal_position, 'name', column_name, 'dataType', column_type, 'udt', data_type,
	  'nullable', (is_nullable='YES'), 'default', IFNULL(column_default,''),
	  'isIdentity', (extra LIKE '%%auto_increment%%'),
	  'isGenerated', (generation_expression IS NOT NULL AND generation_expression<>''),
	  'isPrimaryKey', (column_key='PRI'), 'isUnique', (column_key IN ('PRI','UNI')),
	  'charLen', IFNULL(character_maximum_length,0),
	  'numPrecision', IFNULL(numeric_precision,0), 'numScale', IFNULL(numeric_scale,0)
	)),JSON_ARRAY()) FROM information_schema.columns WHERE table_schema=%s AND table_name=%s`,
		sqlLit(schema), sqlLit(table))
	var raw []myCol
	if err := a.queryJSON(ctx, c, "", colQ, &raw); err != nil {
		return dgTableMeta{}, err
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].Ord < raw[j].Ord })

	// Single-column foreign keys.
	fks := map[string]*dgFK{}
	var fkRows []struct {
		Column    string `json:"column"`
		RefSchema string `json:"refSchema"`
		RefTable  string `json:"refTable"`
		RefColumn string `json:"refColumn"`
	}
	a.queryJSON(ctx, c, "", fmt.Sprintf(`SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT(
	  'column',column_name,'refSchema',referenced_table_schema,'refTable',referenced_table_name,'refColumn',referenced_column_name)),JSON_ARRAY())
	  FROM information_schema.key_column_usage
	  WHERE table_schema=%s AND table_name=%s AND referenced_table_name IS NOT NULL`,
		sqlLit(schema), sqlLit(table)), &fkRows)
	for _, f := range fkRows {
		fks[f.Column] = &dgFK{Schema: f.RefSchema, Table: f.RefTable, Column: f.RefColumn}
	}

	cols := make([]dgColumn, 0, len(raw))
	for _, m := range raw {
		col := dgColumn{
			Name: m.Name, DataType: m.DataType, UDT: m.UDT,
			Nullable: m.Nullable, Default: m.Default,
			IsIdentity: m.IsIdentity, IsGenerated: m.IsGenerated,
			IsPrimaryKey: m.IsPrimaryKey, IsUnique: m.IsUnique,
			CharLen: m.CharLen, NumPrecision: m.NumPrecision, NumScale: m.NumScale,
			FK: fks[m.Name],
		}
		if m.UDT == "enum" || m.UDT == "set" {
			col.Enum = parseEnumType(m.DataType)
		}
		col.Generators = generatorChoices(col, meta)
		col.Generator = inferGenerator(col, meta)
		cols = append(cols, col)
	}
	meta.Columns = cols
	return meta, nil
}

var enumMemberRe = regexp.MustCompile(`'((?:[^']|'')*)'`)

// parseEnumType extracts members from a MySQL enum/set column type, e.g.
// enum('active','inactive') → ["active","inactive"].
func parseEnumType(colType string) []string {
	var out []string
	for _, m := range enumMemberRe.FindAllStringSubmatch(colType, -1) {
		out = append(out, unquoteSQL(m[1]))
	}
	return out
}

func unquoteSQL(s string) string {
	r := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' && i+1 < len(s) && s[i+1] == '\'' {
			r = append(r, '\'')
			i++
			continue
		}
		r = append(r, s[i])
	}
	return string(r)
}
