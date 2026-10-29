package core

import "testing"

// A dial started with some settings left out gets the defaults, and the table
// it was given travels with them.
func TestWithDefaults(t *testing.T) {
	d := Dial{Params: []Param{{Key: "rows", Def: 1000}, {Key: "hold", Def: 120}}}
	p := withDefaults(d, Params{"hold": 30.0, "table": "big_2g", "junk": 1})
	if p.Int("rows", 0) != 1000 || p.Int("hold", 0) != 30 || p.Str("table", "") != "big_2g" {
		t.Errorf("got %v", p)
	}
	if _, ok := p["junk"]; ok {
		t.Error("an unknown setting was kept")
	}
}

func TestPercentile(t *testing.T) {
	if pct(nil, .99) != 0 || pct([]float64{1, 2, 3, 4}, .5) != 2 || pct([]float64{1, 2, 3, 4}, .99) != 3 {
		t.Error("pct")
	}
}
