package gobi

import "testing"

// fakeStats implements the Stats interface with a fixed table of
// per-column bounds so CanPossiblyMatch tests don't need real
// parquet fixtures.
type fakeStats struct {
	minV, maxV map[string]any
	nulls      map[string]int64
	total      int64
}

func (s *fakeStats) MinMax(col string) (any, any, bool) {
	mn, ok1 := s.minV[col]
	mx, ok2 := s.maxV[col]
	if !ok1 || !ok2 {
		return nil, nil, false
	}
	return mn, mx, true
}
func (s *fakeStats) NullCount(col string) (int64, bool) {
	n, ok := s.nulls[col]
	return n, ok
}
func (s *fakeStats) TotalRows() int64 { return s.total }

// intStats builds a Stats for a single Int64 column named col with
// the given min/max bounds and no nulls.
func intStats(col string, minV, maxV int64) *fakeStats {
	return &fakeStats{
		minV:  map[string]any{col: minV},
		maxV:  map[string]any{col: maxV},
		nulls: map[string]int64{col: 0},
		total: 100,
	}
}

// -- Comparison pruning --------------------------------------------------

func TestCanPossiblyMatch_ColGtLit(t *testing.T) {
	// col in [0, 10]  vs  col > 5   → possible (max=10 > 5)
	if !CanPossiblyMatch(Col("x").Gt(Lit(int64(5))), intStats("x", 0, 10)) {
		t.Fatal("col in [0,10] > 5 should be possible")
	}
	// col in [0, 10]  vs  col > 100 → impossible (max=10 not > 100)
	if CanPossiblyMatch(Col("x").Gt(Lit(int64(100))), intStats("x", 0, 10)) {
		t.Fatal("col in [0,10] > 100 should be pruned")
	}
}

func TestCanPossiblyMatch_ColLtLit(t *testing.T) {
	// col in [50, 100]  vs  col < 10  → impossible (min=50 not < 10)
	if CanPossiblyMatch(Col("x").Lt(Lit(int64(10))), intStats("x", 50, 100)) {
		t.Fatal("col in [50,100] < 10 should be pruned")
	}
}

func TestCanPossiblyMatch_ColEqLit(t *testing.T) {
	// col in [0, 10]  vs  col == 5   → possible
	if !CanPossiblyMatch(Col("x").Eq(Lit(int64(5))), intStats("x", 0, 10)) {
		t.Fatal("col in [0,10] == 5 should be possible")
	}
	// col in [0, 10]  vs  col == 42  → impossible
	if CanPossiblyMatch(Col("x").Eq(Lit(int64(42))), intStats("x", 0, 10)) {
		t.Fatal("col in [0,10] == 42 should be pruned")
	}
}

func TestCanPossiblyMatch_LiteralFlipsComparison(t *testing.T) {
	// 100 > col in [0, 10] === col < 100 → possible.
	if !CanPossiblyMatch(Lit(int64(100)).Gt(Col("x")), intStats("x", 0, 10)) {
		t.Fatal("100 > col in [0,10] should be possible")
	}
	// 5 > col in [50, 100] === col < 5 → impossible.
	if CanPossiblyMatch(Lit(int64(5)).Gt(Col("x")), intStats("x", 50, 100)) {
		t.Fatal("5 > col in [50,100] should be pruned")
	}
}

func TestCanPossiblyMatch_AndBothPrunable(t *testing.T) {
	// col in [0, 10]  vs  col > 100 AND col < -5 → impossible
	pred := Col("x").Gt(Lit(int64(100))).And(Col("x").Lt(Lit(int64(-5))))
	if CanPossiblyMatch(pred, intStats("x", 0, 10)) {
		t.Fatal("both conjuncts unsatisfiable → predicate pruned")
	}
}

func TestCanPossiblyMatch_AndOneMatchable(t *testing.T) {
	// col in [0, 10] vs  col > 5 AND col < 20  → possible
	pred := Col("x").Gt(Lit(int64(5))).And(Col("x").Lt(Lit(int64(20))))
	if !CanPossiblyMatch(pred, intStats("x", 0, 10)) {
		t.Fatal("both conjuncts satisfiable → predicate possible")
	}
}

func TestCanPossiblyMatch_OrEitherMatchable(t *testing.T) {
	// col in [0, 10]  vs  col > 100 OR col > 5  → possible (via right side)
	pred := Col("x").Gt(Lit(int64(100))).Or(Col("x").Gt(Lit(int64(5))))
	if !CanPossiblyMatch(pred, intStats("x", 0, 10)) {
		t.Fatal("OR with one satisfiable branch should be possible")
	}
}

func TestCanPossiblyMatch_OrBothUnmatchable(t *testing.T) {
	// col in [0, 10]  vs  col > 100 OR col < -5  → impossible
	pred := Col("x").Gt(Lit(int64(100))).Or(Col("x").Lt(Lit(int64(-5))))
	if CanPossiblyMatch(pred, intStats("x", 0, 10)) {
		t.Fatal("OR with both branches unsatisfiable → pruned")
	}
}

func TestCanPossiblyMatch_UnknownColumnConservative(t *testing.T) {
	// Referenced column not in stats → conservative "possibly matches".
	if !CanPossiblyMatch(Col("nope").Gt(Lit(int64(0))), intStats("x", 0, 10)) {
		t.Fatal("unknown column should be conservative (possibly matches)")
	}
}

func TestCanPossiblyMatch_NoBoundsConservative(t *testing.T) {
	// Column exists but stats.MinMax returns ok=false → conservative.
	s := &fakeStats{
		minV: map[string]any{}, // no minmax entries
		maxV: map[string]any{},
		nulls: map[string]int64{},
		total: 50,
	}
	if !CanPossiblyMatch(Col("x").Gt(Lit(int64(0))), s) {
		t.Fatal("missing stats → conservative (possibly matches)")
	}
}

func TestCanPossiblyMatch_StringRange(t *testing.T) {
	s := &fakeStats{
		minV:  map[string]any{"region": "AT"},
		maxV:  map[string]any{"region": "DE"},
		total: 100,
	}
	// "US" > "DE" so col=="US" should prune.
	if CanPossiblyMatch(Col("region").Eq(Lit("US")), s) {
		t.Fatal("region==US outside [AT..DE] should be pruned")
	}
	if !CanPossiblyMatch(Col("region").Eq(Lit("BE")), s) {
		t.Fatal("region==BE within [AT..DE] should be possible")
	}
}

func TestCanPossiblyMatch_MixedTypesConservative(t *testing.T) {
	// col in [0, 10] as int64  vs  col > "5"  → cmpVal returns 0
	// (incompatible types). Should treat as possibly matches.
	if !CanPossiblyMatch(Col("x").Gt(Lit("5")), intStats("x", 0, 10)) {
		t.Fatal("cross-type comparison should be conservative")
	}
}

func TestCanPossiblyMatch_NotNodeConservative(t *testing.T) {
	// NOT (col > 100) is treated conservatively even though we could
	// invert. Not our job at this layer.
	pred := Col("x").Gt(Lit(int64(100))).Not()
	if !CanPossiblyMatch(pred, intStats("x", 0, 10)) {
		t.Fatal("NOT expressions should be conservative")
	}
}

func TestCanPossiblyMatch_LiteralFalse(t *testing.T) {
	// A bare Lit(false) can never match anything.
	if CanPossiblyMatch(Lit(false), intStats("x", 0, 10)) {
		t.Fatal("Lit(false) should always prune")
	}
}

func TestCanPossiblyMatch_NilPredicate(t *testing.T) {
	// Zero-value Expr → conservative pass-through.
	if !CanPossiblyMatch(Expr{}, intStats("x", 0, 10)) {
		t.Fatal("nil predicate should be conservative")
	}
}

func TestCanPossiblyMatch_NilStats(t *testing.T) {
	if !CanPossiblyMatch(Col("x").Gt(Lit(int64(0))), nil) {
		t.Fatal("nil stats should be conservative")
	}
}
