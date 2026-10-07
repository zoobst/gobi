package athenaio

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// TestBucketResultsFromS3URIs_IsInPredicate — an IsIn expression as
// ReadOptions.Predicate prunes the bucket file's row groups on the
// S3 read path: only the groups whose min/max can hold a listed id
// are decoded. Pruning only; rows within a kept group all come back.
func TestBucketResultsFromS3URIs_IsInPredicate(t *testing.T) {
	ids := make([]int64, 10)
	for i := range ids {
		ids[i] = int64(i)
	}
	df, err := gobi.NewFrameFromSeries(gobi.NewInt64Series("device_id", ids, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()
	var buf bytes.Buffer
	if err := parquetio.Write(df, &buf, &parquetio.WriteOptions{RowGroupRows: 2}); err != nil {
		t.Fatal(err)
	}

	c, err := NewClient(ClientConfig{
		Workgroup: "wg", ResultLocation: "s3://test-bucket/results/",
		Database: "test_db",
		Athena:   &mockCTASAthena{},
		S3:       &mockS3{objects: map[string][]byte{"b/000000_0.parquet": buf.Bytes()}},
		Glue:     &mockGlue{},
	})
	if err != nil {
		t.Fatal(err)
	}
	pred := gobi.Col("device_id").IsIn([]int64{3, 9, 100})
	results, err := c.BucketResultsFromS3URIs(context.Background(),
		[]string{"s3://test-bucket/b/000000_0.parquet"}, &parquetio.ReadOptions{Predicate: pred})
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := results[0].Frame.Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer pruned.Release()
	col, _ := pruned.Column("device_id")
	got, _ := col.Int64s()
	slices.Sort(got)
	if !slices.Equal(got, []int64{2, 3, 8, 9}) {
		t.Errorf("pruned read = %v, want row groups [2 3] and [8 9]", got)
	}

	// The same Expr as a row filter keeps exactly the listed ids.
	filtered, err := results[0].Frame.Filter(pred).Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer filtered.Release()
	col, _ = filtered.Column("device_id")
	got, _ = col.Int64s()
	slices.Sort(got)
	if !slices.Equal(got, []int64{3, 9}) {
		t.Errorf("filtered = %v, want [3 9]", got)
	}
}
