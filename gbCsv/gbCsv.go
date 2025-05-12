package gbcsv

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"log"
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
	arrowcsv "github.com/apache/arrow/go/v18/arrow/csv"
	"github.com/apache/arrow/go/v18/arrow/memory"
)

// holds parameters to be passed around
type csvExplorer struct {
	timeFormat string
	timeCol    int
	headerRow  []string
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
		builder, err := readers.BuildersFromTypes(r.Type)
		if err != nil {
			return nil, err
		}
		builderArray = append(builderArray, builder)
	}

	if *options.HasHeader {
		_, err = genericReader.Read()
		if err != nil {
			return nil, err
		}
	}

	var (
		i          int
		sliceSkips [2]int
	)
	if options.SkipSlice != nil {
		sliceSkips = *options.SkipSlice
	} else {
		sliceSkips = defaultSkipSlice
	}
	for ; ; i++ {
		r, err := genericReader.Read()
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return nil, err
			}
		}
		if i >= sliceSkips[0] && i < sliceSkips[1] {
			continue
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
				if f, err := strconv.ParseFloat(field, 64); err != nil {
					return nil, err
				} else {
					t.Append(f)
				}
			case *array.BinaryBuilder:
				t.Append([]byte(field))
			case *array.Int64Builder:
				if i, err := strconv.ParseInt(field, 10, 64); err == nil {
					t.Append(i)
				} else {
					return nil, err
				}
			case *array.BooleanBuilder:
				if b, err := strconv.ParseBool(field); err == nil {
					t.Append(b)
				} else {
					return nil, err
				}
			case *array.ExtensionBuilder:
				parsed, err := gTypes.ParseStringGeometry(field)
				if err != nil {
					return nil, fmt.Errorf("failed to parse geometry: %w", err) // field is like: "POLYGON ((...))"
				}
				log.Println("identifying parsed's type")
				switch parsed.(type) {
				case gTypes.Polygon:
					log.Println("guess its a polygon")
					// Cast to Polygon type, serialize to WKB
					polygon, ok := parsed.(gTypes.Polygon)
					if !ok {
						log.Println(parsed.Type())
						log.Println("type:", t.Type())
						log.Println("name:", t.Type().Name())
						return nil, fmt.Errorf("not a Polygon type")
					}
					t.StorageBuilder().(*array.BinaryBuilder).Append(polygon.WKB()) // This goes into the Binary storage behind the ExtensionBuilder
				case gTypes.Point:
					point, ok := parsed.(gTypes.Point)
					if !ok {
						return nil, fmt.Errorf("not a Point type")
					}
					t.StorageBuilder().(*array.BinaryBuilder).Append(point.WKB()) // This goes into the Binary storage behind the ExtensionBuilder
				case gTypes.LineString:
					linestring, ok := parsed.(gTypes.LineString)
					if !ok {
						return nil, fmt.Errorf("not a LineString type")
					}
					t.StorageBuilder().(*array.BinaryBuilder).Append(linestring.WKB()) // This goes into the Binary storage behind the ExtensionBuilder
				case *gTypes.GeometryType:
					log.Println("its a geometrytype")
					gType, ok := parsed.(*gTypes.GeometryType)
					if !ok {
						return nil, fmt.Errorf("not a GeometryType")
					}
					t.StorageBuilder().(*array.BinaryBuilder).Append(gType.WKB())
					log.Println(gType.WKB())
				}
			}
		}
	}

	columnList := []arrow.Column{}

	for idx, p := range builderArray {
		var arr arrow.Array
		if p.Type().String() == "extension<Geometry>" {
			arr = gTypes.GeometryType{}.NewArray(*p.(*array.ExtensionBuilder).NewArray().Data().(*array.Data))
		} else {
			arr = p.NewArray()
		}
		log.Println(arr)
		field := genericReader.Schema.Field(idx)

		log.Println(arr.Data())
		log.Println(arr.DataType())

		log.Println("schema type:", field)
		log.Println(genericReader.Schema)
		columnList = append(columnList, arrow.NewColumnFromArr(field, arr))
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

	if *options.InferSchema {
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
			typeMap[idx] = gTypes.Point{}
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

func ReadCSVUsingArrow[T any](t T, path string, options ...arrowcsv.Option) (*gTypes.DataFrame, error) {
	schema, err := readers.CSVStructToArrowSchema(t)
	log.Println("t:", t)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	arrowReader := arrowcsv.NewReader(file, schema, options...)
	defer arrowReader.Release()

	dataRecords := []arrow.Record{}

	for arrowReader.Next() {
		dataRecords = append(dataRecords, arrowReader.Record())
		if arrowReader.Err() != nil {
			return nil, arrowReader.Err()
		}
	}

	table := array.NewTableFromRecords(schema, dataRecords)
	return gTypes.NewDataFrameFromTable(table), nil
}
