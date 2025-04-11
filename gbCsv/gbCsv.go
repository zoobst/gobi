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

	"github.com/apache/arrow/go/arrow"
)

func ReadCsv(path string, options CsvReadOptions) (gTypes.Frame, error) {
	var (
		timeFormat string
		timeCol    int
		headerRow  []string
	)
	df := gTypes.NewDataFrame()

	file, err := os.ReadFile(path)
	if err != nil {
		return df, err
	}

	err = handleCompression(options.Compression, &file, false)
	if err != nil {
		return df, err
	}

	csvReader := csv.NewReader(bytes.NewReader(file))
	if options.Separator != nil {
		csvReader.Comma = *options.Separator
	}

	if options.CommentPrefix != nil {
		csvReader.Comment = *options.CommentPrefix
	}

	if len(*options.Columns) > 0 {
		csvReader.FieldsPerRecord = len(*options.Columns)
	}

	if *options.HasHeader {
		headerRow, err = csvReader.Read()
		if err != nil {
			return df, err
		}
		err = handleHeader(df, headerRow, *options.Columns) // read the first row
		if err != nil {
			return df, err
		}
	}
	// Skip initial rows if specified
	for range *options.SkipRows {
		_, err := csvReader.Read() // Skip row
		if err != nil {
			return df, err
		}
	}

	firstRecord, err := csvReader.Read()
	if err != nil {
		return df, err
	}

	// TODO: Fix all this
	if *options.TryToParseDates {
		for col, r := range firstRecord {
			timeFormat, err = tryToParseDate(r)
			if err != nil {
				return df, err
			}
			timeCol = col
		}
	}

	if options.Schema == nil {
		*options.InferSchema = true
	} else {
		if !checkSchema(options.Schema, firstRecord) {
			return df, berrors.ErrSchemaMismatch
		}
	}

	if *options.InferSchema == true {
		options.Schema, err = inferSchema(firstRecord, headerRow)
	}

	var rows [][]string
	rowCount := 0
	for {
		// Read each row
		record, err := csvReader.Read()
		if err == io.EOF {
			break // End of file reached
		}
		if err != nil {
			if *options.IgnoreErrors {
				continue // Skip this row on error
			}
			return df, err
		}

		// Apply skipping slice logic if necessary
		if *options.SkipSlice != [2]int{-1, -1} {
			start, end := options.SkipSlice[0], options.SkipSlice[1]
			if start >= 0 && end > start && rowCount >= start && rowCount <= end {
				continue // Skip this row based on the SkipSlice
			}
		}

		rowCount++
		if *options.NumRows != -1 && rowCount >= *options.NumRows {
			break // Stop reading if we have reached the desired number of rows
		}
	}

	return df, nil
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
	if len(schema) > 0 {
		for idx, val := range row {
			if t, ok := schema[val]; ok {
				df.Table[idx] = gTypes.Series{
					Type:  t,
					Index: idx,
				}
			} else {
				return berrors.ErrSchemaMismatch
			}
		}
	} else {
		for idx, val := range row {
			df.Series[val] = gTypes.Series{
				Index: idx,
			}
		}
	}
	return nil
}

func tryToParseDate(s string) (format string, err error) {
	for _, format := range TimeFormatsList {
		if _, err = time.Parse(format, s); err == nil {
			return format, nil
		}
	}
	return "", err
}

func checkSchema(schema *arrow.Schema, record []string) bool {

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

func inferType(s string) string {
	// Try parsing as date
	if _, err := tryToParseDate(s); err == nil {
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

	}

	// Default to string
	return "string"
}
