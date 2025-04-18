package gbcsv

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	berrors "github.com/zoobst/gobi/bErrors"
	"github.com/zoobst/gobi/cmprssn"
	gTypes "github.com/zoobst/gobi/globalTypes"
	"github.com/zoobst/gobi/readers"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
)

// holds parameters to be passed around
type csvExplorer struct {
	timeFormat string
	timeCol    int
	headerRow  []string
	reader     readers.CSVReader
	df         gTypes.Frame
	schema     *arrow.Schema
}

func ReadCsv(path string, options CsvReadOptions) (gTypes.Frame, error) {
	exp := csvExplorer{}

	options.setDefaults()

	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	err = handleCompression(options.Compression, &file, false)
	if err != nil {
		return nil, err
	}

	rows := bytes.Split(file, []byte("\n"))

	if *options.HasHeader {
		exp.headerRow = strings.Split(string(rows[0]), string(*options.Separator))
		rows = rows[1:]
	}

	rows = rows[*options.SkipRows:]
	rows1 := rows[:options.SkipSlice[0]]
	rows2 := rows[options.SkipSlice[1]:]
	rows = append(rows1, rows2...)

	firstRecord := strings.Split(string(rows[0]), string(*options.Separator))

	if *options.TryToParseDates {
		exp.timeCol, exp.timeFormat, err = tryToParseDate(string(rows[0]), string(*options.Separator))
		if err != nil {
			return nil, err
		}
	}

	if !checkSchema(options.Schema, firstRecord) {
		return nil, berrors.ErrSchemaMismatch
	}

	if *options.InferSchema == true {
		options.Schema, err = inferSchema(firstRecord, exp.headerRow, string(*options.Separator))
		if err != nil {
			return nil, err
		}
	}

	allocator := memory.NewGoAllocator()

	var builderArray []array.Builder

	// make builders for each col and make coumns
	// with those
	for i, r := range options.Schema.Fields() {
		r.Type
	}

	// figure out builders
	recBuilder := array.NewBuilder(memory.DefaultAllocator, options.Schema)
	defer recBuilder.Release()

	array.NewRecord(options.Schema)

	array.NewTableFromRecords(options.Schema)
}

func handleCompression(compression *cmprssn.CompressionType, data *[]byte, compress bool) error {
	cType := *compression
	switch c := cType.(type) {
	case *cmprssn.GzipCompression:
		if compress {
			c.Compress(data)
		} else {
			c.Decompress(data)
		}
	case nil:
		return nil
	default:
		return berrors.ErrUnsupportedCompressionType
	}
}

func handleHeader(df *gTypes.DataFrame, row []string, schema []string) error {
	return nil
}

func tryToParseDate(s string, sep string) (col int, format string, err error) {
	cols := strings.Split(s, sep)
	for idx, col := range cols {
		for _, format := range TimeFormatsList {
			if _, err = time.Parse(format, col); err == nil {
				return idx, format, nil
			}
		}
	}
	return 0, "", err
}

func checkSchema(schema *arrow.Schema, record []string) bool {

}

func inferSchema(record []string, headers []string, sep string) (*arrow.Schema, error) {
	typeMap := make(map[int]arrow.DataType)
	for idx, feature := range record {
		switch inferType(feature, sep) {
		case "date":
			typeMap[idx] = &arrow.Date64Type{}
		case "float":
			typeMap[idx] = &arrow.Float64Type{}
		case "string":
			typeMap[idx] = &arrow.StringType{}
		case "bool":
			typeMap[idx] = &arrow.BooleanType{}
		case "int":
			typeMap[idx] = &arrow.Int64Type{}
		case "geometry":
			typeMap[idx] = &arrow.ExtensionBase{}
		default:
			typeMap[idx] = &arrow.StringType{}
		}
	}

	var fieldsList []arrow.Field

	for key, val := range typeMap {
		var name string
		if len(headers) >= key {
			name = headers[key]
		} else {
			name = fmt.Sprintf("%d", key)
		}
		newField := arrow.Field{
			Name:     name,
			Type:     val,
			Nullable: true,
			Metadata: arrow.Metadata{},
		}
		fieldsList = append(fieldsList, newField)
	}
	schema := arrow.NewSchema(fieldsList, &arrow.Metadata{})
	return schema, nil
}

func inferType(s, sep string) string {
	// Try parsing as date
	if _, _, err := tryToParseDate(s, sep); err == nil {
		return "date"
	}

	// Try parsing as float
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return "float"
	}

	// Try parsing as int
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return "int"
	}

	// Try parsing as bool
	if strings.ToLower(s) == "true" || strings.ToLower(s) == "false" {
		return "bool"
	}

	if gTypes.CheckGeometry(s) {
		return "geometry"
	}

	// Default to string
	return "string"
}
