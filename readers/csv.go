package readers

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"log"
	"reflect"
	"strings"

	"github.com/apache/arrow/go/v18/arrow"
	gTypes "github.com/zoobst/gobi/globalTypes"
)

type CSVReader struct {
	csv.Reader
	FirstRecord []byte
	Schema      *arrow.Schema
}

type GenericCSVReader[T any] struct {
	CSVReader
}

func NewGenericCSVReader[T any](t T, b *[]byte) (reader GenericCSVReader[T], err error) {
	reader.CSVReader.Reader = *csv.NewReader(bytes.NewBuffer(*b))
	reader.FirstRecord = bytes.Split(*b, []byte("\n"))[1]
	reader.Schema, err = reader.CSVStructToArrowSchema(t)
	log.Println("t:", t)
	if err != nil {
		return reader, err
	}

	return reader, nil
}

func (self GenericCSVReader[T]) CSVStructToArrowSchema(s any) (schema *arrow.Schema, err error) {
	val := reflect.ValueOf(s)
	log.Println("val:", val)
	typ := val.Type()
	log.Println("typ:", typ)
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("expected struct, got %s", typ.Kind())
	}

	var fields []arrow.Field
	for i := range typ.NumField() {
		field := typ.Field(i)
		log.Println("field:", field)

		csvTag := field.Tag.Get("csv")
		dTypeTag := field.Tag.Get("dtype")

		if csvTag == "" {
			continue // skip if tag is missing
		}

		if dTypeTag == "geometry" {
			if err = arrow.RegisterExtensionType(gTypes.Point{}); err != nil {
				if !strings.Contains(err.Error(), "already defined") {
					return nil, err
				}
			}
			if err = arrow.RegisterExtensionType(gTypes.Polygon{}); err != nil {
				if !strings.Contains(err.Error(), "already defined") {
					return nil, err
				}
			}
			if err = arrow.RegisterExtensionType(gTypes.LineString{}); err != nil {
				if !strings.Contains(err.Error(), "already defined") {
					return nil, err
				}
			}
			if err = arrow.RegisterExtensionType(&gTypes.GeometryType{}); err != nil {
				if !strings.Contains(err.Error(), "already defined") {
					return nil, err
				}
			}
			extType := arrow.GetExtensionType("Geometry")
			if extType == nil {
				return nil, fmt.Errorf("extension type 'Geometry' not registered")
			}
			fields = append(fields, arrow.Field{Name: csvTag, Type: extType, Nullable: true})
			continue
		}

		arrowType, err := ArrowTypeFromGo(typ.Field(i).Type)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name, err)
		}

		fields = append(fields, arrow.Field{Name: csvTag, Type: arrowType, Nullable: true})
	}
	log.Println("fields:", fields)

	newSchema := arrow.NewSchema(fields, nil)
	log.Println("newSchema:", newSchema)
	return newSchema, nil
}

func CSVStructToArrowSchema(s any) (schema *arrow.Schema, err error) {
	val := reflect.ValueOf(s)
	log.Println("val:", val)
	typ := val.Type()
	log.Println("typ:", typ)
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("expected struct, got %s", typ.Kind())
	}

	var fields []arrow.Field
	for i := range typ.NumField() {
		field := typ.Field(i)
		log.Println("field:", field)

		csvTag := field.Tag.Get("csv")
		dTypeTag := field.Tag.Get("dtype")

		if csvTag == "" {
			continue // skip if tag is missing
		}

		if dTypeTag == "geometry" {
			if err = arrow.RegisterExtensionType(gTypes.Point{}); err != nil {
				if !strings.Contains(err.Error(), "already defined") {
					return nil, err
				}
			}
			if err = arrow.RegisterExtensionType(gTypes.Polygon{}); err != nil {
				if !strings.Contains(err.Error(), "already defined") {
					return nil, err
				}
			}
			if err = arrow.RegisterExtensionType(gTypes.LineString{}); err != nil {
				if !strings.Contains(err.Error(), "already defined") {
					return nil, err
				}
			}
			if err = arrow.RegisterExtensionType(&gTypes.GeometryType{}); err != nil {
				if !strings.Contains(err.Error(), "already defined") {
					return nil, err
				}
			}
			extType := arrow.GetExtensionType("Geometry")
			if extType == nil {
				return nil, fmt.Errorf("extension type 'Geometry' not registered")
			}
			fields = append(fields, arrow.Field{Name: csvTag, Type: extType, Nullable: true})
			continue
		}

		arrowType, err := ArrowTypeFromGo(typ.Field(i).Type)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name, err)
		}

		fields = append(fields, arrow.Field{Name: csvTag, Type: arrowType, Nullable: true})
	}
	log.Println("fields:", fields)

	newSchema := arrow.NewSchema(fields, nil)
	log.Println("newSchema:", newSchema)
	return newSchema, nil
}

func extractWKTFromCSV(record []byte) (string, error) {
	reader := csv.NewReader(bytes.NewReader(record))
	reader.TrimLeadingSpace = true
	reader.LazyQuotes = true

	fields, err := reader.Read()
	if err != nil {
		return "", fmt.Errorf("failed to parse CSV: %w", err)
	}

	for _, field := range fields {
		field = strings.TrimSpace(field)
		// Match common WKT types
		if strings.HasPrefix(field, "POINT") ||
			strings.HasPrefix(field, "LINESTRING") ||
			strings.HasPrefix(field, "POLYGON") {
			return field, nil
		}
	}

	return "", fmt.Errorf("no WKT geometry found in record: %s", record)
}
