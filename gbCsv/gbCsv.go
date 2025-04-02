package gbcsv

import (
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"time"

	"github.com/zoobst/gobi/cmprssn"

	gTypes "github.com/zoobst/gobi/globalTypes"

	berrors "github.com/zoobst/gobi/bErrors"

	"github.com/apache/arrow/go/arrow"
)

func ReadCsv(path string, options CsvReadOptions) (*gTypes.DataFrame, error) {
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
		headerRow, err := csvReader.Read()
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

	// Optionally parse dates if the flag is enabled
	// TODO: Fix all this
	var (
		timeFormat string
		timeCol    int
		t          bool = true
	)

	if *options.TryToParseDates {
		timeFormat, timeCol, err = tryToParseDates(firstRecord)
		if err != nil {
			return df, err
		}
	}

	if options.Schema == nil {
		options.InferSchema = &t
		if err != nil {
			return df, err
		}
	} else {
		if !checkSchema(options.Schema, firstRecord) {
			return df, berrors.ErrSchemaMismatch
		}
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

func handleHeader(df *gTypes.DataFrame, row []string, schema map[string]gTypes.GBType) error {
	if len(schema) > 0 {
		for idx, val := range row {
			if t, ok := schema[val]; ok {
				df.Columns[val] = gTypes.Series{
					Type:  t,
					Index: idx,
				}
			} else {
				return berrors.ErrSchemaMismatch
			}
		}
	} else {
		for idx, val := range row {
			df.Columns[val] = gTypes.Series{
				Index: idx,
			}
		}
	}
	return nil
}

func tryToParseDates(row []string) (format string, col int, err error) {
	for col, value := range row {
		for _, format := range TimeFormatsList {
			_, err = time.Parse(format, value)
			if err == nil {
				return format, col, nil
			}
		}
	}
	return "", 0, err
}

func checkSchema(schema *arrow.Schema, record []string) bool {

}

func inferSchema(record []string) (*arrow.Schema, error) {

}
