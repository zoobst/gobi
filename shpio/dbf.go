package shpio

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// -----------------------------------------------------------------------------
// dBase III (.dbf) reader
//
// Header layout (32 bytes):
//   [0]    version byte (0x03 = dBase III without memo)
//   [1-3]  YY MM DD last update
//   [4-7]  record count (uint32 LE)
//   [8-9]  header size (uint16 LE)
//   [10-11] record size (uint16 LE)
//   [12-31] reserved / flags
//
// After the header, each field descriptor is 32 bytes:
//   [0-10]  field name (ASCII, null-terminated, up to 11 bytes)
//   [11]    type: 'C' 'N' 'F' 'L' 'D' 'M' ...
//   [12-15] field data address (unused for our purposes)
//   [16]    field length
//   [17]    decimal count
//   [18-31] reserved
//
// Field descriptors end with a 0x0D terminator byte.
//
// Records: each starts with a 1-byte deletion flag (' ' = live, '*' =
// deleted) followed by field-length bytes per column. Numeric / date /
// logical fields are ASCII text padded with spaces.
// -----------------------------------------------------------------------------

// parseDBF returns per-column Arrow fields + arrays for the .dbf blob. n
// is the number of records the caller expects (used to sanity-check
// against the DBF header's own count).
func parseDBF(data []byte, expectedRecords int) ([]arrow.Field, []arrow.Array, error) {
	if len(data) < 32 {
		return nil, nil, fmt.Errorf("%w: dbf header too short", ErrInvalidShapefile)
	}
	numRecords := int(binary.LittleEndian.Uint32(data[4:8]))
	headerSize := int(binary.LittleEndian.Uint16(data[8:10]))
	recordSize := int(binary.LittleEndian.Uint16(data[10:12]))

	if expectedRecords > 0 && numRecords != expectedRecords {
		// Not fatal — shapefiles occasionally disagree slightly at the tail.
		// Use whichever is smaller so we don't run off either buffer.
		if numRecords > expectedRecords {
			numRecords = expectedRecords
		}
	}

	// Parse field descriptors: 32 bytes each, terminated by 0x0D at
	// position (fieldStart + n*32).
	type fieldDesc struct {
		name   string
		typ    byte
		length int
	}
	var fields []fieldDesc
	off := 32
	for off < headerSize && off < len(data) {
		if data[off] == 0x0D {
			break
		}
		if off+32 > len(data) {
			return nil, nil, fmt.Errorf("%w: dbf field descriptor truncated", ErrInvalidShapefile)
		}
		name := trimField(data[off : off+11])
		typ := data[off+11]
		length := int(data[off+16])
		fields = append(fields, fieldDesc{name: name, typ: typ, length: length})
		off += 32
	}

	// Build one Arrow builder per field.
	pool := memory.DefaultAllocator
	builders := make([]array.Builder, len(fields))
	arrowFields := make([]arrow.Field, len(fields))
	for i, f := range fields {
		af, b := dbfFieldToArrow(pool, f.name, f.typ)
		arrowFields[i] = af
		builders[i] = b
	}
	defer func() {
		for _, b := range builders {
			b.Release()
		}
	}()

	// Walk record rows.
	if recordSize <= 0 {
		return nil, nil, fmt.Errorf("%w: dbf record size = 0", ErrInvalidShapefile)
	}
	for r := 0; r < numRecords; r++ {
		recOff := headerSize + r*recordSize
		if recOff+recordSize > len(data) {
			// Truncated file: stop early, emit as many rows as we can read.
			break
		}
		// Skip deletion flag byte (recOff).
		colOff := recOff + 1
		for i, f := range fields {
			raw := data[colOff : colOff+f.length]
			if err := appendDBFCell(builders[i], f.typ, raw); err != nil {
				return nil, nil, err
			}
			colOff += f.length
		}
	}

	arrs := make([]arrow.Array, len(builders))
	for i, b := range builders {
		arrs[i] = b.NewArray()
	}
	return arrowFields, arrs, nil
}

// dbfFieldToArrow maps a dBase III field type + name to an Arrow field +
// builder. C → String, N/F → Float64 (safest across decimal / integer
// mixes), L → Boolean, D → String (ISO date), other → String.
func dbfFieldToArrow(pool memory.Allocator, name string, typ byte) (arrow.Field, array.Builder) {
	switch typ {
	case 'N', 'F':
		return arrow.Field{Name: name, Type: arrow.PrimitiveTypes.Float64, Nullable: true},
			array.NewFloat64Builder(pool)
	case 'L':
		return arrow.Field{Name: name, Type: arrow.FixedWidthTypes.Boolean, Nullable: true},
			array.NewBooleanBuilder(pool)
	case 'D':
		// dBase dates are YYYYMMDD ASCII. Expose as String for now to
		// avoid guessing the caller's timezone.
		return arrow.Field{Name: name, Type: arrow.BinaryTypes.String, Nullable: true},
			array.NewStringBuilder(pool)
	default:
		// 'C', 'M', and anything else fall back to string.
		return arrow.Field{Name: name, Type: arrow.BinaryTypes.String, Nullable: true},
			array.NewStringBuilder(pool)
	}
}

// appendDBFCell parses one cell of a DBF row and appends it to b.
func appendDBFCell(b array.Builder, typ byte, raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		b.AppendNull()
		return nil
	}
	switch tb := b.(type) {
	case *array.Float64Builder:
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			// dBase leaves blank / "***" for unrepresentable values; treat as null.
			tb.AppendNull()
			return nil
		}
		tb.Append(v)
	case *array.BooleanBuilder:
		switch s {
		case "T", "t", "Y", "y", "1":
			tb.Append(true)
		case "F", "f", "N", "n", "0":
			tb.Append(false)
		default:
			tb.AppendNull()
		}
	case *array.StringBuilder:
		tb.Append(s)
	default:
		return fmt.Errorf("%w: unhandled builder type %T for dbf type %c", ErrInvalidShapefile, b, typ)
	}
	_ = typ
	return nil
}

// trimField returns the ASCII field name, stopping at the first null byte
// or trailing space. dBase III field names are up to 10 useful bytes plus
// a null terminator.
func trimField(b []byte) string {
	i := 0
	for i < len(b) && b[i] != 0 && b[i] != ' ' {
		i++
	}
	return string(b[:i])
}
