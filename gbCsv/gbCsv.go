package gbcsv

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
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

func ReadFromGeneric[T any](t T, path string, options CsvReadOptions) (*gTypes.DataFrame, error) {
	options.setDefaults()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	genericReader, err := readers.NewGenericCSVReader(t, &b)
	if err != nil {
		return nil, err
	}

	var builderArray []array.Builder

	for _, r := range genericReader.Schema.Fields() {
		switch r.Type {
		case arrow.PrimitiveTypes.Float64:
			builderArray = append(builderArray, array.NewFloat64Builder(memory.DefaultAllocator))
		case arrow.PrimitiveTypes.Int64:
			builderArray = append(builderArray, array.NewInt64Builder(memory.DefaultAllocator))
		case arrow.FixedWidthTypes.Boolean:
			builderArray = append(builderArray, array.NewBooleanBuilder(memory.DefaultAllocator))
		case arrow.BinaryTypes.String:
			builderArray = append(builderArray, array.NewStringBuilder(memory.DefaultAllocator))
		case arrow.FixedWidthTypes.Date64:
			builderArray = append(builderArray, array.NewDate64Builder(memory.DefaultAllocator))
		case arrow.BinaryTypes.Binary:
			builderArray = append(builderArray, array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary))
		case gTypes.GenericPolygon():
			builderArray = append(builderArray, array.NewExtensionBuilder(memory.DefaultAllocator, gTypes.GenericPolygon()))
		case gTypes.GenericLineString():
			builderArray = append(builderArray, array.NewExtensionBuilder(memory.DefaultAllocator, gTypes.GenericLineString()))
		case gTypes.GenericPoint():
			builderArray = append(builderArray, array.NewExtensionBuilder(memory.DefaultAllocator, gTypes.GenericPoint()))
		default:
			builderArray = append(builderArray, array.NewStringBuilder(memory.DefaultAllocator))
		}
	}

	if *options.HasHeader {
		_, err = genericReader.Read()
		if err != nil {
			return nil, err
		}
	}

	for {
		r, err := genericReader.Read()
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return nil, err
			}
		}

		for idx, field := range r {
			if field == "" {
				builderArray[idx].AppendNull()
				continue
			}
			switch t := builderArray[idx].(type) {
			case *array.StringBuilder:
				t.Append(field)
			case *array.Float64Builder:
				f, err := strconv.ParseFloat(field, 64)
				if err != nil {
					return nil, err
				}
				t.Append(f)
			case *array.BinaryBuilder:
				t.Append([]byte(field))
			case *array.Int64Builder:
				i, err := strconv.ParseInt(field, 10, 64)
				if err != nil {
					return nil, err
				}
				t.Append(i)
			case *array.BooleanBuilder:
				b, err := strconv.ParseBool(field)
				if err != nil {
					return nil, err
				}
				t.Append(b)
			}
		}
	}

	columnList := []arrow.Column{}

	for idx, p := range builderArray {
		arr := p.NewArray()
		columnList = append(columnList, arrow.NewColumnFromArr(genericReader.Schema.Field(idx), arr))
	}

	df, err := gTypes.NewDataFrameFromColumns(columnList, genericReader.Schema)
	if err != nil {
		return nil, err
	}

	return df, nil
}

func ReadCsv(path string, options CsvReadOptions) (*gTypes.DataFrame, error) {
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
		csvReader := csv.NewReader(bytes.NewReader(rows[0]))
		csvReader.Comma = *options.Separator
		csvReader.Comment = *options.CommentPrefix
		row, err := csvReader.ReadAll()
		if err != nil {
			return nil, err
		}

		exp.headerRow = row[0]
		rows = rows[1:]
	}

	rows = rows[*options.SkipRows:]
	if options.SkipSlice != nil {
		rows1 := rows[:options.SkipSlice[0]]
		rows2 := rows[options.SkipSlice[1]:]
		rows = append(rows1, rows2...)
	}

	data := []byte{}

	for _, r := range rows {
		data = append(data, r...)
	}

	csvReader := csv.NewReader(bytes.NewReader(data))
	csvReader.Comma = *options.Separator
	csvReader.Comment = *options.CommentPrefix

	firstRecord, err := csvReader.Read()
	if err != nil {
		return nil, err
	}

	if *options.TryToParseDates {
		exp.timeCol, exp.timeFormat, err = tryToParseDate(firstRecord)
		if err != nil {
			return nil, err
		}
	}

	if *options.InferSchema == true {
		options.Schema, err = inferSchema(firstRecord, exp.headerRow)
		if err != nil {
			return nil, err
		}
	}

	var builderArray []array.Builder

	for _, r := range options.Schema.Fields() {
		builderArray = append(builderArray, array.NewBuilder(memory.DefaultAllocator, r.Type))
	}

	for {
		r, err := csvReader.Read()
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return nil, err
			}
		}

		for idx, field := range r {
			if field == "" {
				builderArray[idx].AppendNull()
			}
			builderArray[idx].AppendValueFromString(field)
		}
	}

	df := gTypes.NewDataFrame(options.Schema)

	for idx, p := range builderArray {
		arr := p.NewArray()
		df.AddColumn(idx, options.Schema.Field(idx), arrow.NewColumnFromArr(options.Schema.Field(idx), arr))
	}
	return &df, nil
}

func handleCompression(compression *cmprssn.CompressionType, data *[]byte, compress bool) error {
	if compression == nil {
		return nil
	}
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
	return nil
}

func handleHeader(df *gTypes.DataFrame, row []string, schema []string) error {
	return nil
}

func tryToParseDate(s []string) (col int, format string, err error) {
	for idx, col := range s {
		for _, format := range TimeFormatsList {
			if _, err = time.Parse(format, col); err == nil {
				return idx, format, nil
			}
		}
	}
	return 0, "", err
}

func checkSchema(schema *arrow.Schema, record []string) bool {
	return false
}

func inferSchema(record []string, headers []string) (*arrow.Schema, error) {
	typeMap := make(map[int]arrow.DataType)
	for idx, feature := range record {
		switch inferType(feature) {
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
		if len(headers)-1 >= key {
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

func inferType(s string) string {
	// Try parsing as date
	if _, _, err := tryToParseDate([]string{s}); err == nil {
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
