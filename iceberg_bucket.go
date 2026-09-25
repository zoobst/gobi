package gobi

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"unsafe"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// IcebergBucket returns an expression computing Iceberg's bucket(n)
// partition transform of e: (murmur3_x86_32(v) & MaxInt32) % n, with v
// serialized the way the Iceberg spec (and iceberg-go) hashes it.
//
//   - Int8 / Int16 / Int32 / Int64 and Date32: the value as a
//     little-endian 8-byte long (Iceberg int, long, date).
//   - Timestamp in s / ms / µs: converted exactly to microseconds, then
//     as a long (Iceberg timestamp / timestamptz). Nanosecond
//     timestamps are rejected: iceberg-go has no timestamp_ns bucket
//     to match.
//   - String / LargeString: UTF-8 bytes. Binary / LargeBinary /
//     FixedSizeBinary: raw bytes (Iceberg string, binary, fixed, and
//     uuid as 16 fixed bytes).
//
// Output is Int32; null in, null out. n must be positive. Unsigned and
// floating-point columns have no Iceberg bucket and are rejected, as
// are decimals (not implemented).
func (e Expr) IcebergBucket(n int) Expr {
	return Expr{node: &icebergBucketNode{inner: e.node, n: n}}
}

type icebergBucketNode struct {
	inner ExprNode
	n     int
}

func (nd *icebergBucketNode) Eval(input *Frame) (Series, error) {
	if nd.inner == nil {
		return Series{}, fmt.Errorf("gobi: IcebergBucket on nil inner expression")
	}
	s, err := nd.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	return s.IcebergBucket(nd.n)
}

func (nd *icebergBucketNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	t, err := nd.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	if err := checkIcebergBucket(t, nd.n); err != nil {
		return nil, err
	}
	return arrow.PrimitiveTypes.Int32, nil
}

func (nd *icebergBucketNode) Children() []Expr { return []Expr{{node: nd.inner}} }
func (nd *icebergBucketNode) String() string {
	return fmt.Sprintf("%s.iceberg_bucket(%d)", nd.inner, nd.n)
}

func checkIcebergBucket(dt arrow.DataType, n int) error {
	if n <= 0 || n > math.MaxInt32 {
		return fmt.Errorf("gobi: IcebergBucket: bucket count %d must be in [1, %d]", n, math.MaxInt32)
	}
	switch dt.ID() {
	case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64, arrow.DATE32,
		arrow.STRING, arrow.LARGE_STRING, arrow.BINARY, arrow.LARGE_BINARY, arrow.FIXED_SIZE_BINARY:
		return nil
	case arrow.TIMESTAMP:
		if dt.(*arrow.TimestampType).Unit == arrow.Nanosecond {
			return fmt.Errorf("%w: IcebergBucket: nanosecond timestamps have no Iceberg bucket to match; cast to microseconds first",
				ErrExprTypeMismatch)
		}
		return nil
	}
	return fmt.Errorf("%w: IcebergBucket: no Iceberg bucket transform for %s", ErrExprTypeMismatch, dt)
}

// IcebergBucket computes Iceberg's bucket(n) transform of every value
// in s. See Expr.IcebergBucket for the per-type rules.
func (s Series) IcebergBucket(n int) (Series, error) {
	if s.col == nil {
		return Series{}, fmt.Errorf("gobi: IcebergBucket on empty series")
	}
	if err := checkIcebergBucket(s.DataType(), n); err != nil {
		return Series{}, err
	}
	pool := memory.DefaultAllocator
	b := array.NewInt32Builder(pool)
	defer b.Release()
	b.Reserve(s.Len())
	bucket := func(h uint32) int32 { return (int32(h) & math.MaxInt32) % int32(n) }
	var buf [8]byte
	long := func(v int64) int32 {
		binary.LittleEndian.PutUint64(buf[:], uint64(v))
		return bucket(murmur3Sum32(buf[:]))
	}
	for _, chunk := range s.col.Data().Chunks() {
		for i := range chunk.Len() {
			if chunk.IsNull(i) {
				b.AppendNull()
				continue
			}
			switch a := chunk.(type) {
			case *array.Int8:
				b.Append(long(int64(a.Value(i))))
			case *array.Int16:
				b.Append(long(int64(a.Value(i))))
			case *array.Int32:
				b.Append(long(int64(a.Value(i))))
			case *array.Int64:
				b.Append(long(a.Value(i)))
			case *array.Date32:
				b.Append(long(int64(a.Value(i))))
			case *array.Timestamp:
				us, err := timestampMicros(int64(a.Value(i)), a.DataType().(*arrow.TimestampType).Unit)
				if err != nil {
					return Series{}, err
				}
				b.Append(long(us))
			case *array.String:
				b.Append(bucket(murmur3Sum32String(a.Value(i))))
			case *array.LargeString:
				b.Append(bucket(murmur3Sum32String(a.Value(i))))
			case *array.Binary:
				b.Append(bucket(murmur3Sum32(a.Value(i))))
			case *array.LargeBinary:
				b.Append(bucket(murmur3Sum32(a.Value(i))))
			case *array.FixedSizeBinary:
				b.Append(bucket(murmur3Sum32(a.Value(i))))
			default:
				return Series{}, fmt.Errorf("%w: IcebergBucket: chunk type %T", ErrExprTypeMismatch, chunk)
			}
		}
	}
	return arrayToSeries(pool, s.name, arrow.PrimitiveTypes.Int32, b.NewArray())
}

// timestampMicros converts a timestamp in unit to microseconds,
// exactly (s / ms scale up; overflow is an error).
func timestampMicros(v int64, unit arrow.TimeUnit) (int64, error) {
	var mult int64
	switch unit {
	case arrow.Microsecond:
		return v, nil
	case arrow.Millisecond:
		mult = 1_000
	case arrow.Second:
		mult = 1_000_000
	default:
		return 0, fmt.Errorf("%w: IcebergBucket: unsupported timestamp unit %s", ErrExprTypeMismatch, unit)
	}
	if v > math.MaxInt64/mult || v < math.MinInt64/mult {
		return 0, fmt.Errorf("gobi: IcebergBucket: timestamp %d%s overflows int64 microseconds", v, unit)
	}
	return v * mult, nil
}

// murmur3Sum32 is MurmurHash3 x86_32 with seed 0 — the hash the
// Iceberg spec's bucket transform uses.
func murmur3Sum32(data []byte) uint32 {
	const (
		c1 = 0xcc9e2d51
		c2 = 0x1b873593
	)
	var h uint32
	n := len(data)
	for len(data) >= 4 {
		k := binary.LittleEndian.Uint32(data)
		data = data[4:]
		k *= c1
		k = bits.RotateLeft32(k, 15)
		k *= c2
		h ^= k
		h = bits.RotateLeft32(h, 13)
		h = h*5 + 0xe6546b64
	}
	var k uint32
	switch len(data) {
	case 3:
		k ^= uint32(data[2]) << 16
		fallthrough
	case 2:
		k ^= uint32(data[1]) << 8
		fallthrough
	case 1:
		k ^= uint32(data[0])
		k *= c1
		k = bits.RotateLeft32(k, 15)
		k *= c2
		h ^= k
	}
	h ^= uint32(n)
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	h *= 0xc2b2ae35
	h ^= h >> 16
	return h
}

// murmur3Sum32String hashes s's bytes without copying.
func murmur3Sum32String(s string) uint32 {
	return murmur3Sum32(unsafe.Slice(unsafe.StringData(s), len(s)))
}
