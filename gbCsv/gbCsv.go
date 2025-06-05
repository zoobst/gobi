package gbcsv

import (
	"fmt"
	"io"
	"log"
	"os"
	"strconv"

	berrors "github.com/zoobst/gobi/bErrors"
	"github.com/zoobst/gobi/cmprssn"
	gTypes "github.com/zoobst/gobi/globalTypes"
	"github.com/zoobst/gobi/readers"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	arrowcsv "github.com/apache/arrow/go/v18/arrow/csv"
)

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

	var (
		builderArray []array.Builder
		i            int
		sliceSkips   [2]int
	)

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

	log.Println("making new arrowcsv Reader...")
	arrowReader := arrowcsv.NewReader(file, schema, options...)
	log.Println("done")
	dataRecords := []arrow.Record{}

	log.Println("starting next loop...")
	arrowReader.Retain()
	defer arrowReader.Release()

	for arrowReader.Next() {
		if err := arrowReader.Err(); err != nil {
			return nil, err
		}
		rec := arrowReader.Record()
		if rec == nil {
			log.Println("nil record")
			continue
		}
		log.Printf("record has %d columns, %d rows", rec.NumCols(), rec.NumRows())
		for i := range int(rec.NumCols()) {
			arr := rec.Column(i)
			log.Printf("  column %d (%s): %d values", i, rec.ColumnName(i), arr.Len())
		}
		dataRecords = append(dataRecords, rec)
	}

	return gTypes.NewDataFrameFromRecords(schema, &dataRecords)
}
