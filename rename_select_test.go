package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// --- LitNull ------------------------------------------------------------

func TestLitNull_StringBroadcast(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.WithColumnExpr("provider", LitNull(arrow.BinaryTypes.String))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := out.Column("provider")
	if err != nil {
		t.Fatal(err)
	}
	if provider.DataType().ID() != arrow.STRING {
		t.Fatalf("provider type = %s, want STRING", provider.DataType())
	}
	arr := provider.col.Data().Chunks()[0].(*array.String)
	for i := range 5 {
		if !arr.IsNull(i) {
			t.Fatalf("row %d not null (LitNull should produce all nulls)", i)
		}
	}
}

func TestLitNull_ComposesWithCollectSet(t *testing.T) {
	f := lazyFrame(t)
	// Adding a null provider column then aggregating: the null-of-type
	// String should be skipped by the set aggregator, yielding an
	// empty list per group.
	out, err := f.Lazy().
		WithColumn("provider", LitNull(arrow.BinaryTypes.String)).
		GroupBy("region").
		Agg(Aggregation{Column: "provider", Fn: NewStringSetAggregator(), Alias: "providers"}).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// Two regions (US, EU), each with empty provider set.
	if r, _ := out.Shape(); r != 2 {
		t.Fatalf("row count = %d, want 2", r)
	}
	providers, _ := out.Column("providers")
	la := providers.col.Data().Chunks()[0].(*array.List)
	for i := 0; i < 2; i++ {
		start, end := la.ValueOffsets(i)
		if end != start {
			t.Fatalf("row %d producer list should be empty; got %d values", i, end-start)
		}
	}
}

func TestLitNull_TypeIsPreserved(t *testing.T) {
	f := lazyFrame(t)
	// Verify Type() reports the requested dtype at plan time.
	lf := f.Lazy().WithColumn("k", LitNull(arrow.PrimitiveTypes.Uint64))
	fields, ok := lf.Schema().FieldsByName("k")
	if !ok || len(fields) == 0 {
		t.Fatalf("k field missing")
	}
	if fields[0].Type.ID() != arrow.UINT64 {
		t.Fatalf("k type = %s, want UINT64", fields[0].Type)
	}
}

// --- SelectCols ---------------------------------------------------------

func TestSelectCols_Eager(t *testing.T) {
	f := lazyFrame(t)
	// Reorder: region first, then price. Drop id and active.
	out, err := f.SelectCols("region", "price")
	if err != nil {
		t.Fatal(err)
	}
	names := out.ColumnNames()
	if len(names) != 2 || names[0] != "region" || names[1] != "price" {
		t.Fatalf("column names = %v, want [region price]", names)
	}
}

func TestSelectCols_MissingColumn(t *testing.T) {
	f := lazyFrame(t)
	_, err := f.SelectCols("region", "nope")
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestSelectCols_Lazy(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.Lazy().SelectCols("region", "id").Collect()
	if err != nil {
		t.Fatal(err)
	}
	names := out.ColumnNames()
	if len(names) != 2 || names[0] != "region" || names[1] != "id" {
		t.Fatalf("column names = %v, want [region id]", names)
	}
}

func TestSelectCols_Empty(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.SelectCols()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ColumnNames()) != 0 {
		t.Fatalf("empty SelectCols should produce a 0-column Frame, got %d columns", len(out.ColumnNames()))
	}
}

// --- Rename -------------------------------------------------------------

func TestRename_EagerPreservesBuffers(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.Rename("price", "cost")
	if err != nil {
		t.Fatal(err)
	}
	// The renamed column should be present under the new name...
	cost, err := out.Column("cost")
	if err != nil {
		t.Fatal(err)
	}
	if cost.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("cost dtype = %s, want FLOAT64", cost.DataType())
	}
	// ...and absent under the old name.
	if _, err := out.Column("price"); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("old name should be gone; got err %v", err)
	}
	// Column order preserved.
	oldNames := f.ColumnNames()
	newNames := out.ColumnNames()
	if len(oldNames) != len(newNames) {
		t.Fatalf("column count changed: %v -> %v", oldNames, newNames)
	}
	// Only the renamed position differs.
	for i := range oldNames {
		want := oldNames[i]
		if oldNames[i] == "price" {
			want = "cost"
		}
		if newNames[i] != want {
			t.Fatalf("column %d: %q, want %q", i, newNames[i], want)
		}
	}
}

func TestRename_MissingErrors(t *testing.T) {
	f := lazyFrame(t)
	_, err := f.Rename("nope", "new")
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestRename_SameNameIsNoop(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.Rename("price", "price")
	if err != nil {
		t.Fatal(err)
	}
	// Frame.Rename with old==new returns the receiver — cheap no-op,
	// matches LazyFrame.Rename's identity path.
	if out != f {
		t.Fatal("Frame.Rename(same, same) should return the receiver unchanged")
	}
}

func TestRename_Lazy(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.Lazy().Rename("price", "cost").Collect()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Column("cost"); err != nil {
		t.Fatalf("cost column missing after lazy rename: %v", err)
	}
	if _, err := out.Column("price"); !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("price should be gone; got err %v", err)
	}
}

func TestRename_LazySameNameNoop(t *testing.T) {
	f := lazyFrame(t)
	// LazyFrame.Rename with old==new returns receiver — the plan tree
	// shouldn't grow a rename node.
	lf := f.Lazy()
	lf2 := lf.Rename("price", "price")
	if lf2 != lf {
		t.Fatal("LazyFrame.Rename(same, same) should be a no-op returning the receiver")
	}
}

// End-to-end: rename + SelectCols + LitNull composing.
func TestRename_ComposedPipeline(t *testing.T) {
	f := lazyFrame(t)
	out, err := f.Lazy().
		Rename("price", "cost").
		WithColumn("provider", LitNull(arrow.BinaryTypes.String)).
		SelectCols("id", "cost", "provider").
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	names := out.ColumnNames()
	if len(names) != 3 || names[0] != "id" || names[1] != "cost" || names[2] != "provider" {
		t.Fatalf("column names = %v, want [id cost provider]", names)
	}
}
