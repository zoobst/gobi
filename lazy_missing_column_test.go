package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
)

// TestLazy_MissingColumnErrorsAtCollect — building a plan that names a
// missing column must not panic (arrow.NewSchema rejects nil-typed
// fields); the error surfaces from Collect / CollectRaw as documented.
func TestLazy_MissingColumnErrorsAtCollect(t *testing.T) {
	df, err := NewFrameFromSeries(NewInt64Series("a", []int64{1, 2}, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()

	for name, build := range map[string]func() *LazyFrame{
		"SelectCols":          func() *LazyFrame { return df.Lazy().SelectCols("a", "nope") },
		"SelectCols only":     func() *LazyFrame { return df.Lazy().SelectCols("nope") },
		"Select(Col)":         func() *LazyFrame { return df.Lazy().Select(Col("nope")) },
		"Select(expr)":        func() *LazyFrame { return df.Lazy().Select(Col("nope").Add(Lit(int64(1)))) },
		"WithColumn":          func() *LazyFrame { return df.Lazy().WithColumn("b", Col("nope")) },
		"after Filter":        func() *LazyFrame { return df.Lazy().Filter(Col("a").Gt(Lit(int64(0)))).SelectCols("nope") },
		"SelectCols then use": func() *LazyFrame { return df.Lazy().SelectCols("nope").Filter(Col("nope").IsNull()) },
	} {
		var lf *LazyFrame
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: plan construction panicked: %v", name, r)
				}
			}()
			lf = build()
		}()
		if lf == nil {
			continue
		}
		if _, err := lf.Collect(); !errors.Is(err, ErrColumnNotFound) {
			t.Errorf("%s: Collect err = %v, want ErrColumnNotFound", name, err)
		}
		if _, err := lf.CollectRaw(); !errors.Is(err, ErrColumnNotFound) {
			t.Errorf("%s: CollectRaw err = %v, want ErrColumnNotFound", name, err)
		}
	}

	// The eager schema marks the unresolved column with the Null type.
	if f := df.Lazy().SelectCols("a", "nope").Schema().Field(1); f.Name != "nope" || f.Type.ID() != arrow.NULL {
		t.Errorf("schema field = %v, want nope: null", f)
	}
}
