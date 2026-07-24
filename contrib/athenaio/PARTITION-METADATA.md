# Partition-Aware `LazyFrame` (gobi core)

**Status:** design in progress. Lives under `contrib/athenaio/`
(gitignored) alongside the athenaio design doc, but this is *gobi
core work* — not athenaio-specific. When published, this doc moves
to `docs/` or gets folded into `CLAUDE.md`. Every future `<vendor>io`
package (bigqueryio, snowflakeio, redshiftio) will land on the
infrastructure this describes; athenaio is just the first caller.

## Purpose

Add `PartitionMetadata` as a first-class `LazyFrame` property that
the optimizer can reason about. Enables three classes of shuffle
elimination:

- **Shuffle-free `.Over(K)`** — window aggregations skip the
  cross-worker partition step when the source is already
  hash-partitioned on K.
- **Partition-wise `Join(K)`** — when both sides are hash-partitioned
  on the join key with the same hash function, skip the global
  build-side hash table (huge RSS win — the build side dominates
  memory on parquet-heavy workloads).
- **Repartition-skip `GroupBy(K).Agg(...)`** — Slice D's parallel
  aggregate currently does its own hash-repartition; alignment
  metadata lets us skip that step entirely when the input is
  already aligned.

## The metadata

```go
package gobi

// PartitionMetadata describes how a LazyFrame's rows are physically
// grouped by partition key. Populated by scan sources that can
// prove partitioning (parquetio with per-file partitioning claims,
// athenaio's CTAS write path, hand-annotated via
// WithPartitionAssertion). Consumed by the optimizer to prove
// alignment for Over / Join / GroupBy and eliminate cross-worker
// shuffles.
//
// Nil PartitionMetadata means "no claim made" — the optimizer
// assumes worst-case (any row can be anywhere) and inserts the
// shuffle it would insert today. A non-nil zero value (Columns == nil)
// is an explicit "no partitioning" claim and is distinct from nil;
// alignment proofs refuse to prove anything against it. Never
// conflate the two.
type PartitionMetadata struct {
    // Columns are the partition columns in the order used by the
    // hash function. hash(a, b) ≠ hash(b, a) in every hash function
    // gobi tags, so ordering is significant. Alignment always
    // requires ordered equality.
    Columns []string

    // HashFn is a versioned, source-namespaced string tag naming
    // the hash function used to partition. Empty string is
    // reserved for value-partitioning (Hive-style directory-per-
    // unique-value) where "same value → same partition" is trivial.
    //
    // Cross-tag comparisons always fail — the optimizer never
    // assumes two source's hash functions are compatible even when
    // both are documented as "xxhash64" or "murmur3". Alignment
    // holds iff HashFn strings are byte-equal.
    //
    // Reserved tags:
    //   ""                                  — value partitioning
    //   "gobi/xxhash64/v1"                  — gobi runtime shuffle
    //   "athenaio/iceberg/murmur3-32/v1"    — athenaio Iceberg CTAS
    //   "athenaio/hive/bucket/v1"           — athenaio Hive CTAS
    //   "pgio/hash/v1"                      — future pgio (reserved)
    //   "bigqueryio/hash/v1"                — future bigqueryio (reserved)
    HashFn string

    // SortedBy describes within-partition ordering. Nil = unsorted.
    // Each SortKey is a (column, direction) pair — direction
    // matters for Diff / Shift / fill-forward correctness.
    SortedBy []SortKey

    // SortEnforced distinguishes "writer guaranteed the sort"
    // (e.g. Iceberg's first-class sorted_by) from "sort was a hint,
    // verify at read time or don't rely on it for correctness"
    // (e.g. Hive's sorted_by, which is advisory).
    //
    // Operators that use sortedness only for performance (skipping
    // a re-sort) may trust either value. Operators whose
    // correctness depends on order (Diff, Shift with fill_forward)
    // MUST check this flag and either verify at read time or fall
    // back to a sorted path.
    SortEnforced bool
}

type SortKey struct {
    Column     string
    Descending bool // false = ascending; matches SQL ORDER BY default
}
```

**Storage.** Attached to `scanFrameNode` / `scanFileNode` in the
plan tree at construction time. Propagates through the plan tree
(see below). Reachable via `LazyFrame.PartitionMetadata()` for
introspection + debugging.

## Alignment proof

The alignment predicate: given a `LazyFrame` with `PartitionMetadata`
and an operator that consumes it with key columns K, does the
alignment guarantee let us skip the shuffle?

**First-cut rule (exact match only):**

```
Aligned(meta, K, hashFn) ⇔
    meta != nil                            AND
    meta.HashFn == hashFn                  AND
    equal_ordered(meta.Columns, K)
```

- **`meta != nil`.** Nil metadata = no claim, refuse to prove.
  Non-nil-but-empty is an explicit "no partitioning" claim that
  still fails the columns equality check.
- **`HashFn` byte-equal.** No cross-tag matching. Different source
  hash functions cannot be assumed compatible even if the underlying
  algorithm is the same, because the type-encoding of hash inputs
  differs across sources (Trino's hash normalizes decimals
  differently than xxhash64 over UTF-8 bytes, for example).
- **Ordered set equality on `Columns`.** `hash(a, b) ≠ hash(b, a)`.
  `.Over(K)` where K matches partition columns in the same order
  aligns; any other permutation doesn't.

**Refuse anything looser in v1:**

- No subset matching (`.Over(A)` on `hash(A, B)` partitioning fails
  even though A is a prefix of the partition key — the hash
  distributes over the joint value, not the prefix).
- No FD-based aliasing (`.Over("city")` on `hash("region")`
  partitioning fails even if city functionally determines region —
  reasoning about that requires schema-level knowledge gobi doesn't
  have and shouldn't try to invent).

**Modulus is orthogonal.** `hash(K) % N` aligns with `.Over(K)`
regardless of N. The bucket count is a granularity knob; the
"same K → same bucket" property is preserved for any N ≥ 1. Worth
spelling out in the operator docs because it's the common case.

## Escape hatch: `WithPartitionAssertion`

For opaque sources (RawUnload / RawCTAS / hand-crafted parquet
scans / custom UDFs that write partitioned output) where athenaio
or parquetio can't infer the metadata:

```go
// WithPartitionAssertion attaches user-provided PartitionMetadata
// to a LazyFrame. Only narrowing is allowed — the assertion
// cannot widen an existing metadata claim inferred from the
// source. Widening (e.g. asserting a source with HashFn=""
// value-partitioning is actually "athenaio/iceberg/murmur3-32/v1"
// hash-partitioned) is refused with an error.
//
// User owns correctness. gobi never verifies the assertion against
// actual data — it trusts the claim and uses it in alignment
// proofs. A wrong assertion produces wrong window results with no
// visible error, so use with care.
func (lf *LazyFrame) WithPartitionAssertion(meta PartitionMetadata) *LazyFrame
```

**Narrowing-only rationale:** if athenaio's `UnloadAndRead` claims
`hash(K) on athenaio/iceberg/murmur3-32/v1` and the user calls
`.WithPartitionAssertion({Columns: ["K"], HashFn: ""})` to claim
value partitioning, that's contradicting the source. Refuse.
Assertion is for making claims about opaque sources, not for
overriding sources that told you their partitioning.

Narrowing examples that are OK:
- Dropping `SortedBy` claim (user knows the sort isn't preserved
  downstream).
- Reducing `Columns` from `["A", "B"]` to `["A"]` — never useful
  actually, since hash(A, B) does not imply hash(A). This is
  strictly a widening in disguise. Refuse it too.

The practically useful narrowing: replacing `SortEnforced: true`
with `SortEnforced: false` after a transform that might have
disturbed the sort. Everything else is either widening (refuse) or
a full replacement (only allowed on nil source metadata).

## Propagation rules

Every plan node's `Schema()` already computes an output schema.
Add a parallel `PartitionMetadata()` method that computes the
output metadata from inputs. Rules:

| Operator            | Metadata behavior                                                                                    |
|---------------------|------------------------------------------------------------------------------------------------------|
| `ScanFrame`         | Nil (materialized frames make no partitioning claim by default).                                     |
| `ScanFile`          | Sourced from the scan node (parquetio + athenaio populate; other sources default nil).               |
| `Filter`            | Preserved. Removing rows preserves same-K-still-in-same-partition.                                   |
| `Project`           | Preserved iff all `Columns` and all `SortedBy` columns survive the projection; else dropped.         |
| `WithColumn`        | Preserved. Adding a column doesn't disturb existing partitioning.                                    |
| `Drop`              | Preserved iff dropped column is not in `Columns` or `SortedBy`; else dropped.                        |
| `Limit(n)`          | `Columns` + `HashFn` preserved. `SortedBy` preserved only if `SortEnforced == true` (a Limit on an unsorted claim discards the sortedness portion). Non-obvious — see below. |
| `Sort(K')`          | `Columns` + `HashFn` preserved iff Sort is a per-partition sort (v2 concept — v1 sort is global, so this always drops partitioning). `SortedBy` replaced with K'. |
| `GroupBy(K).Agg`    | Dropped entirely. Output rows are groups, not input rows; no relationship to input partitioning.     |
| `Join(K)`           | See below (complex).                                                                                 |
| `Explode`           | Preserved iff exploded column is not in `Columns`; else dropped. `SortedBy` invalidated regardless (explode changes row cardinality per parent, breaks any within-partition sort claim). |
| `Over(K).Eval`      | Not a plan node in the current design (Over is an Expr, not an operator). Consumes metadata via the alignment predicate; does not produce it. |

### `Limit(n)` subtlety

`Limit(n)` on a partition-aligned + sorted LazyFrame preserves the
partition claim (row subset preserves same-K-in-same-partition)
and preserves the `SortedBy` claim only if the sort was
writer-enforced. Why: if `SortEnforced == false`, the sort was a
hint that operators skip-optimize against; Limit may take rows
from any file, so a *global* sorted-ness claim across partitions
was never real in the first place. The right thing is to strip
`SortedBy` on `Limit` when `SortEnforced == false` — represent
what's actually true, not what was hinted.

### `Join(K, K')` rules

Given two inputs L and R with metadata `L.meta` and `R.meta`:

- **Partition-wise join eligible** iff:
  - `L.meta != nil` AND `R.meta != nil`
  - `L.meta.HashFn == R.meta.HashFn` (byte-equal)
  - `equal_ordered(L.meta.Columns, [K])` AND `equal_ordered(R.meta.Columns, [K'])`
  - (Plus `K == K'` semantically — the join keys must be the same
    column values, which the executor already checks at type
    inference time.)
- **Output metadata:** for an Inner join, both sides preserve
  their partition claim (the joined output is still hash-partitioned
  on the same key). For a Left/Right/Full outer, the surviving
  side's claim is preserved for that side's rows; document that
  the unmatched-side rows have unknown partitioning (mark output
  metadata nil to be safe — the win is inside the join itself, not
  downstream).

Partition-wise join is a real perf win (skip the global build-side
hash table) but implementing it means teaching the join executor
about per-partition build tables. That's a larger slice of work than
the alignment predicate itself. First deliverable: alignment
metadata + Over consumer + Join *plans* the partition-wise strategy
in `Explain` but doesn't yet execute it differently. Actual
executor rewiring is a follow-up.

## Consumer: `.Over(K)`

The Over ExprNode already exists (see `expr_over.go`). Rewire its
`Eval` to consult the input `LazyFrame`'s `PartitionMetadata`:

- **Aligned case:** each worker's input batches are already
  homogeneous on K. Skip the row → group-id partitioner in Over;
  just aggregate per K within the batch, and every worker's output
  is disjoint from every other worker's. No cross-worker step.
- **Unaligned case:** fall back to today's behavior — row-order
  partitioner + scatter, single-worker path (or the future
  partitioned-shuffle path when we build it).

Detection happens at `Compile` time via the alignment predicate.
No runtime overhead if the alignment doesn't hold.

## Testing story

Unit tests exercise the alignment predicate + propagation rules as
pure functions of `PartitionMetadata` — no executor needed. A
representative table:

- Every propagation rule: input metadata + operator → expected
  output metadata.
- Every alignment case: input metadata + (K, HashFn) → expected
  Aligned decision.
- Every widening/narrowing case on `WithPartitionAssertion`: assertion
  + source metadata → expected error or new metadata.

Executor integration tests (in gobi core) exercise `.Over(K)` +
`Join(K)` + `GroupBy(K)` against a manually-constructed
`LazyFrame` that carries `WithPartitionAssertion`-provided
metadata. Verifies the shuffle-skip actually kicks in (via
`ExplainPhysical` output) and produces correct results.

## Sequencing

The gobi-core work is a prerequisite for athenaio T3. Order of
operations:

1. **Land `PartitionMetadata` type + attach to scan nodes.** No
   consumer wiring yet — sources populate but nothing uses it.
2. **Propagation rules through the plan tree.** Every operator's
   `PartitionMetadata()` method. Unit tests only.
3. **Alignment predicate + `WithPartitionAssertion`.** Pure
   function, unit-testable.
4. **`.Over(K)` consumer rewired.** Executor integration tests
   using `WithPartitionAssertion`-constructed frames. First
   user-visible win.
5. **athenaio T1 (lifecycle wrapper).** No `PartitionMetadata`
   claim; RawQuery only.
6. **athenaio T3 (partition-aware `UnloadAndRead`).** Populates
   `PartitionMetadata` from CTAS spec + read-back verification.
   End-to-end: athenaio + gobi.Over integration test proves
   shuffle-skip against a real Athena result.
7. **Partition-wise `Join` execution.** Alignment metadata is
   already produced by earlier work; this slice teaches the join
   executor to use it. Independent, can slip to v0.4 if scope is
   tight.
8. **Repartition-skip `GroupBy`.** Same shape as partition-wise
   join — infrastructure exists, executor rewiring is the work.

Steps 1-4 are gobi core. Step 5 is athenaio-only. Step 6 needs
both. Steps 7-8 are follow-ups.

## Open questions

- **Where does `PartitionMetadata()` live on the plan interface?**
  Add to `LogicalPlan` interface (breaking every node) or a
  separate optional interface with type-assertion checks?
  Leaning toward `LogicalPlan` — every node has to answer the
  question sooner or later, and gobi doesn't have public
  `LogicalPlan` implementers outside the package.
- **Multiple partition metadata claims.** Can a `LazyFrame` be
  simultaneously partitioned on multiple key sets (e.g., a source
  that's `hash(A, B)` partitioned AND `hash(A)` partitioned as a
  weaker claim)? Would enable more alignment matches. Deferring;
  v1 is single-claim. Users needing multiple claims can chain
  `WithPartitionAssertion` — actually no, that only narrows.
  Punt to v2.
- **Explain output.** `ExplainPhysical` should surface the
  partition claim so users can debug "why did Over shuffle?"
  Concrete format TBD but probably a one-line annotation per node:
  `Scan(...) [partition: hash(K)/N, sorted(ts asc), enforced]`.
