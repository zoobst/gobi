package parquetio

import (
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/metadata"

	"github.com/zoobst/gobi"
)

// rowGroupStats adapts a parquet row-group's footer metadata to
// gobi.Stats, so gobi.CanPossiblyMatch can evaluate a predicate
// against min/max/null-count bounds without importing parquet
// internals into gobi.
//
// The name→ordinal map is built once when the reader is opened; per-
// row-group instances share it via a pointer so predicate-pushdown
// work is O(#predicates × #row-groups) rather than O(#columns ×
// #row-groups).
type rowGroupStats struct {
	rg        *metadata.RowGroupMetaData
	colByName map[string]int
	// covering maps a geometry column to the flat columns its
	// GeoParquet covering.bbox declares (xmin, ymin, xmax, ymax).
	covering map[string][4]string
}

// CoveringColumns implements gobi.CoveringStats: spatial pruning reads
// whatever columns the file's covering declares — e.g. lon / lat for a
// point file — rather than assuming gobi's generated names.
func (s *rowGroupStats) CoveringColumns(geom string) (xmin, ymin, xmax, ymax string, ok bool) {
	c, ok := s.covering[geom]
	return c[0], c[1], c[2], c[3], ok
}

// declaredCoverings extracts flat (single-element-path) bbox coverings
// from a file's "geo" footer entry. Malformed or absent metadata
// yields nil, and pruning falls back to the generated names.
func declaredCoverings(pf *file.Reader) map[string][4]string {
	raw := pf.MetaData().KeyValueMetadata().FindValue(gobi.GeoParquetMetadataKey)
	if raw == nil {
		return nil
	}
	meta, err := gobi.ParseGeoParquetMetadata(*raw)
	if err != nil || meta == nil {
		return nil
	}
	out := map[string][4]string{}
	for geom, cm := range meta.Columns {
		if cm.Covering == nil || cm.Covering.Bbox == nil {
			continue
		}
		bb := cm.Covering.Bbox
		if len(bb.Xmin) != 1 || len(bb.Ymin) != 1 || len(bb.Xmax) != 1 || len(bb.Ymax) != 1 {
			continue // nested covering: not addressable by flat column stats
		}
		out[geom] = [4]string{bb.Xmin[0], bb.Ymin[0], bb.Xmax[0], bb.Ymax[0]}
	}
	return out
}

func (s *rowGroupStats) TotalRows() int64 { return s.rg.NumRows() }

func (s *rowGroupStats) MinMax(col string) (any, any, bool) {
	idx, ok := s.colByName[col]
	if !ok {
		return nil, nil, false
	}
	cc, err := s.rg.ColumnChunk(idx)
	if err != nil {
		return nil, nil, false
	}
	stats, err := cc.Statistics()
	if err != nil || stats == nil || !stats.HasMinMax() {
		return nil, nil, false
	}
	return decodeMinMax(stats)
}

func (s *rowGroupStats) NullCount(col string) (int64, bool) {
	idx, ok := s.colByName[col]
	if !ok {
		return 0, false
	}
	cc, err := s.rg.ColumnChunk(idx)
	if err != nil {
		return 0, false
	}
	stats, err := cc.Statistics()
	if err != nil || stats == nil || !stats.HasNullCount() {
		return 0, false
	}
	return stats.NullCount(), true
}

// decodeMinMax pulls Go-typed min/max scalars from a TypedStatistics.
// Returns ok=false for types gobi.CanPossiblyMatch can't compare
// (Int96, FixedLenByteArray outside strings, etc.).
func decodeMinMax(stats metadata.TypedStatistics) (any, any, bool) {
	switch s := stats.(type) {
	case *metadata.Int32Statistics:
		return s.Min(), s.Max(), true
	case *metadata.Int64Statistics:
		return s.Min(), s.Max(), true
	case *metadata.Float32Statistics:
		return s.Min(), s.Max(), true
	case *metadata.Float64Statistics:
		return s.Min(), s.Max(), true
	case *metadata.BooleanStatistics:
		return s.Min(), s.Max(), true
	case *metadata.ByteArrayStatistics:
		// ByteArray covers STRING and BINARY; the parquet-arrow layer
		// converts to Go string for STRING-typed columns.
		return string(s.Min()), string(s.Max()), true
	}
	return nil, nil, false
}

// buildColByName maps top-level column names to their parquet
// leaf-column indices. Used by rowGroupStats to look up ColumnChunk
// entries by user-facing name.
//
// Flat schemas only: gobi doesn't emit nested types, so name-to-leaf
// mapping is straightforward. Nested schemas would need a path walk.
func buildColByName(pf *file.Reader) map[string]int {
	sch := pf.MetaData().Schema
	out := make(map[string]int, sch.NumColumns())
	for i := 0; i < sch.NumColumns(); i++ {
		out[sch.Column(i).Name()] = i
	}
	return out
}

// filterRowGroupsByPredicate walks pf's row-groups and returns the
// subset whose min/max stats don't prove the predicate impossible.
// A nil or unusable predicate keeps every row-group (fallback to the
// caller's original selection).
func filterRowGroupsByPredicate(pf *file.Reader, pred gobi.Expr, candidates []int) []int {
	if pred.Node() == nil {
		return candidates
	}
	colByName := buildColByName(pf)
	covering := declaredCoverings(pf)
	kept := make([]int, 0, len(candidates))
	for _, rgIdx := range candidates {
		rg := pf.MetaData().RowGroup(rgIdx)
		s := &rowGroupStats{rg: rg, colByName: colByName, covering: covering}
		if gobi.CanPossiblyMatch(pred, s) {
			kept = append(kept, rgIdx)
		}
	}
	return kept
}
