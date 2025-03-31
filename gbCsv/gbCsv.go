package gbcsv

import (
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/zoobst/gobi/cmprssn"

	gTypes "github.com/zoobst/gobi/globalTypes"

	berrors "github.com/zoobst/gobi/bErrors"

	"github.com/apache/arrow/go/arrow"
)

func (cro *CsvReadOptions) Default() *CsvReadOptions {
	return &CsvReadOptions{
		HasHeader:       true,
		Columns:         make(map[string]gTypes.GBType),
		Separator:       ',',
		CommentPrefix:   '#',
		QuoteChar:       '"',
		SkipRows:        0,
		SkipSlice:       [2]int{-1, -1},
		InferSchema:     true,
		Schema:          &arrow.Schema{},
		SchemaOverrides: make(map[string]any),
		NullValues:      nil,
		IgnoreErrors:    false,
		TryToParseDates: false,
		MaxWorkers:      runtime.NumCPU() / 2,
		BatchSize:       8192,
		NumRows:         -1,
		Encoding:        &gTypes.Utf8Type,
		SampleSize:      1024,
		Compression:     &cmprssn.None{},
	}
}

func ReadCsv(path string, options CsvReadOptions) (*gTypes.DataFrame, error) {
	df := gTypes.NewDataFrame()

	file, err := os.ReadFile(path)
	if err != nil {
		return df, err
	}

	data, err := handleCompression(options, &file, false)
	if err != nil {
		return df, err
	}

	csvReader := csv.NewReader(bytes.NewReader(data)) // TODO: Fix this
	csvReader.Comma = options.Separator
	csvReader.Comment = options.CommentPrefix
	if len(options.Columns) > 0 {
		csvReader.FieldsPerRecord = len(options.Columns)
	}

	if options.HasHeader {
		headerRow, err := csvReader.Read()
		if err != nil {
			return df, err
		}
		err = handleHeader(df, headerRow, options.Columns) // read the first row
		if err != nil {
			return df, err
		}
	}
	// Skip initial rows if specified
	for range options.SkipRows {
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
	)

	if options.TryToParseDates {
		timeFormat, timeCol, err = tryToParseDates(firstRecord)
		if err != nil {
			return df, err
		}
	}

	if len(options.Schema.Fields()) == 0 {
		options.Schema, err = inferSchema(firstRecord)
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
			if options.IgnoreErrors {
				continue // Skip this row on error
			}
			return df, err
		}

		// Apply skipping slice logic if necessary
		if options.SkipSlice != [2]int{-1, -1} {
			start, end := options.SkipSlice[0], options.SkipSlice[1]
			if start >= 0 && end > start && rowCount >= start && rowCount <= end {
				continue // Skip this row based on the SkipSlice
			}
		}

		rowCount++
		if options.NumRows != -1 && rowCount >= options.NumRows {
			break // Stop reading if we have reached the desired number of rows
		}
	}

	return df, nil
}

func handleCompression(options CsvReadOptions, data *[]byte, compress bool) (*[]byte, error) {
	switch c := options.Compression.(type) {
	case *cmprssn.GzipCompression:
		if compress {
			return c.Compress(data)
		} else {
			return c.Decompress(data)
		}
	case *cmprssn.None:
		return c.Compress(data)
	default:
		return nil, berrors.ErrUnsupportedCompressionType
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
