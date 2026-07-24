package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
)

// PartitionMetadata describes how a LazyFrame's rows are physically
// grouped by partition key. Populated by scan sources that can prove
// partitioning (athenaio's CTAS write path, future parquetio
// per-file partitioning claims, hand-annotated via
// WithPartitionAssertion). Consumed by the optimizer to prove
// alignment for Over / Join / GroupBy and eliminate cross-worker
// shuffles.
//
// Two nil-versus-empty distinctions matter:
//
//   - A nil *PartitionMetadata means "no claim made" — the optimizer
//     assumes worst-case (any row can be anywhere) and inserts the
//     shuffle it would insert today.
//   - A non-nil pointer with Columns == nil is an explicit "no
//     partitioning" claim (a source that ran without bucketing) and
//     is distinct from nil. Alignment proofs refuse to prove
//     anything against it. Never conflate the two.
type PartitionMetadata struct {
	// Columns are the partition columns in the order used by the
	// hash function. hash(a, b) ≠ hash(b, a) in every hash function
	// gobi tags, so ordering is significant. Alignment always
	// requires ordered equality on this slice.
	Columns []string

	// HashFn is a versioned, source-namespaced string tag naming
	// the hash function used to partition. Empty string is reserved
	// for value-partitioning (Hive-style directory-per-unique-value)
	// where "same value → same partition" is trivial.
	//
	// Cross-tag comparisons always fail — the optimizer never
	// assumes two sources' hash functions are compatible even when
	// both name the same underlying algorithm. Type-encoding of
	// hash inputs differs across sources (Trino's hash normalizes
	// decimals differently than xxhash64 over UTF-8 bytes, for
	// example). Alignment holds iff HashFn strings are byte-equal.
	//
	// Reserved tags (see PARTITION-METADATA.md for the registry):
	//   ""                                  — value partitioning
	//   "gobi/xxhash64/v1"                  — gobi runtime shuffle
	//   "athenaio/iceberg/murmur3-32/v1"    — athenaio Iceberg CTAS
	//   "athenaio/hive/bucket/v1"           — athenaio Hive CTAS
	//   "pgio/hash/v1"                      — reserved for pgio
	//   "bigqueryio/hash/v1"                — reserved for bigqueryio
	HashFn string

	// SortedBy describes within-partition ordering. Nil = unsorted.
	// Each SortKey is a (column, direction) pair — direction matters
	// for Diff / Shift / fill-forward correctness.
	SortedBy []SortKey

	// SortEnforced distinguishes "writer guaranteed the sort" (e.g.
	// Iceberg's first-class sorted_by table property) from "sort
	// was a hint, verify at read time or don't rely on it for
	// correctness" (e.g. Hive's sorted_by, which is advisory).
	//
	// Operators that use sortedness only for performance (skipping
	// a re-sort) may trust either value. Operators whose correctness
	// depends on order (Diff, Shift with fill_forward) MUST check
	// this flag and either verify at read time or fall back to a
	// sorted path.
	SortEnforced bool
}

// SortKey (defined in sort.go) is reused as the within-partition
// sort element — same (Column, Descending) shape as Frame.SortBy's
// lexicographic key, and reusing the type means Sort-node
// propagation rules in step 2 can carry keys straight through
// without a translation step.

// Aligned reports whether meta's partition claim colocates rows by
// the given columns — the shape the optimizer needs to prove that
// .Over(K) / GroupBy(K) / hash-partition-aware Aggregate can skip
// the cross-worker shuffle.
//
// First-cut rule (v1): exact match only. meta must be non-nil,
// meta.Columns must equal columns in order, and meta's HashFn is
// otherwise unconstrained (any hash — including "" for value
// partitioning — is fine for a single-source alignment check;
// two-source alignment for partition-wise join uses AlignedWith).
//
// Refuses aliasing (via FDs), subset matching (Over("A") on a
// hash(A, B) partition), and column-order permutations. Users who
// need looser matches go through LazyFrame.WithPartitionAssertion
// and own correctness.
func Aligned(meta *PartitionMetadata, columns []string) bool {
	if meta == nil {
		return false
	}
	return stringSlicesEqual(meta.Columns, columns) && len(columns) > 0
}

// AlignedWith reports whether two partition claims describe the
// same physical partitioning — same ordered Columns AND same
// HashFn. Consumed by the partition-wise Join rule (both sides
// must be partitioned on the join key by a byte-equal hash for a
// shuffle-free join to be safe).
//
// Both metas must be non-nil with non-empty Columns; empty-Columns
// claims explicitly represent "no partitioning" and never align
// with anything.
func AlignedWith(l, r *PartitionMetadata) bool {
	if l == nil || r == nil {
		return false
	}
	if len(l.Columns) == 0 || len(r.Columns) == 0 {
		return false
	}
	if l.HashFn != r.HashFn {
		return false
	}
	return stringSlicesEqual(l.Columns, r.Columns)
}

// validateAssertion checks that assertion is a narrowing (or
// equivalent) of src's claim. Returns nil if valid, an error
// naming the widening otherwise.
//
// Semantics:
//   - Nil src → any assertion allowed (opaque source, user provides
//     the whole claim).
//   - Nil assertion → always allowed (narrowing to "no claim").
//   - Non-nil src, non-nil assertion:
//       - Columns must byte-equal (assertion can't change what
//         the source partitions on).
//       - HashFn must byte-equal (can't change hash function).
//       - assertion.SortedBy must be a prefix of src.SortedBy
//         (writer contract that enforced [a, b, c] also enforced
//         [a, b] as a prefix, so shorter prefixes are valid
//         narrowings; different columns or reordered are not).
//       - assertion.SortEnforced may downgrade true→false but not
//         upgrade false→true (upgrading is a stronger claim than
//         the source made).
func validateAssertion(src, assertion *PartitionMetadata) error {
	if src == nil || assertion == nil {
		return nil
	}
	if !stringSlicesEqual(src.Columns, assertion.Columns) {
		return fmt.Errorf("gobi: partition assertion cannot change Columns: source=%v assertion=%v",
			src.Columns, assertion.Columns)
	}
	if src.HashFn != assertion.HashFn {
		return fmt.Errorf("gobi: partition assertion cannot change HashFn: source=%q assertion=%q",
			src.HashFn, assertion.HashFn)
	}
	if !isSortedByPrefix(assertion.SortedBy, src.SortedBy) {
		return fmt.Errorf("gobi: partition assertion SortedBy must be a prefix of source SortedBy: source=%v assertion=%v",
			src.SortedBy, assertion.SortedBy)
	}
	if !src.SortEnforced && assertion.SortEnforced {
		return fmt.Errorf("gobi: partition assertion cannot upgrade SortEnforced from hint to enforced")
	}
	return nil
}

// isSortedByPrefix reports whether candidate is a (possibly empty)
// prefix of full. Empty candidate is always a valid prefix.
func isSortedByPrefix(candidate, full []SortKey) bool {
	if len(candidate) > len(full) {
		return false
	}
	for i, sk := range candidate {
		if sk != full[i] {
			return false
		}
	}
	return true
}

// propagateProjection returns the metadata that survives a Project /
// Drop-shaped column reshaping against outSchema. Rule:
//
//   - Nil input → nil output (no claim to propagate).
//   - Any partition Column missing from outSchema → drop everything
//     (the hash function is opaque; we can't reason about a subset
//     of its inputs).
//   - Otherwise partition claim survives; SortedBy is truncated to
//     its longest surviving prefix (SortEnforced carries with the
//     prefix — a writer that enforced [a, b, c] also enforced [a, b]
//     as a prefix). Zero surviving prefix drops both SortedBy and
//     SortEnforced.
func propagateProjection(in *PartitionMetadata, outSchema *arrow.Schema) *PartitionMetadata {
	if in == nil {
		return nil
	}
	for _, c := range in.Columns {
		if !outSchema.HasField(c) {
			return nil
		}
	}
	kept := 0
	for _, sk := range in.SortedBy {
		if !outSchema.HasField(sk.Column) {
			break
		}
		kept++
	}
	out := in.Clone()
	if kept == 0 {
		out.SortedBy = nil
		out.SortEnforced = false
	} else if kept < len(in.SortedBy) {
		out.SortedBy = out.SortedBy[:kept]
	}
	return out
}

// propagateLimit handles Limit/Tail-shaped row-subset operators.
// Partition claim survives (any row subset preserves same-K-in-
// same-partition). SortedBy survives only when the source enforced
// the sort — hint-only sortedness is meaningless once we've taken
// a subset, so we strip it to prevent downstream operators from
// relying on order that was never guaranteed.
func propagateLimit(in *PartitionMetadata) *PartitionMetadata {
	if in == nil {
		return nil
	}
	if in.SortEnforced || len(in.SortedBy) == 0 {
		return in
	}
	out := in.Clone()
	out.SortedBy = nil
	out.SortEnforced = false
	return out
}

// Clone returns a deep copy of m. Used by plan-tree walkers that
// need to attach a modified metadata to a downstream node without
// mutating the upstream one. Returns nil for a nil receiver.
func (m *PartitionMetadata) Clone() *PartitionMetadata {
	if m == nil {
		return nil
	}
	out := &PartitionMetadata{
		HashFn:       m.HashFn,
		SortEnforced: m.SortEnforced,
	}
	if m.Columns != nil {
		out.Columns = append([]string(nil), m.Columns...)
	}
	if m.SortedBy != nil {
		out.SortedBy = append([]SortKey(nil), m.SortedBy...)
	}
	return out
}
