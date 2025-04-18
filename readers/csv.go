package readers

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"reflect"

	"github.com/apache/arrow/go/v18/arrow"
)

type CSVReader struct {
	csv.Reader
	Schema *arrow.Schema
}

type GenericCSVReader[T any] struct {
	CSVReader
}

func NewGenericCSVReader[T any](t T, b *[]byte) (reader GenericCSVReader[T], err error) {
	reader.CSVReader.Reader = *csv.NewReader(bytes.NewBuffer(*b))
	reader.Schema, err = CSVStructToArrowSchema(t)
	if err != nil {
		return reader, err
	}

	return reader, nil
}

func CSVStructToArrowSchema(s any) (*arrow.Schema, error) {
	val := reflect.ValueOf(s)
	typ := val.Type()

	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("expected struct, got %s", typ.Kind())
	}

	var fields []arrow.Field
	for i := range typ.NumField() {
		field := typ.Field(i)

		csvTag := field.Tag.Get("csv")

		if csvTag == "" {
			continue // skip if tag is missing
		}

		arrowType, err := ArrowTypeFromGo(typ.Field(i).Type)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name, err)
		}

		fields = append(fields, arrow.Field{Name: csvTag, Type: arrowType, Nullable: true})
	}

	return arrow.NewSchema(fields, nil), nil
}
