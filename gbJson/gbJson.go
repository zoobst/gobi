package gbJson

import (
	"fmt"
	"os"

	berrors "github.com/zoobst/gobi/bErrors"
	"github.com/zoobst/gobi/geojson"
	gTypes "github.com/zoobst/gobi/globalTypes"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
)

func ReadGeoJSON(path string, options GeoJSONReadOptions) (df *gTypes.DataFrame, err error) {
	df = gTypes.NewDataFrame()

	file, err := os.Open(path)
	if err != nil {
		return df, err
	}
	defer file.Close()

	var buf []byte

	_, err = file.Read(buf)
	if err != nil {
		return df, err
	}

	fc, err := geojson.UnmarshalGeoJSON(&buf)
	if err != nil {
		return df, err
	}

	schema := createArrowSchemaFromProperties(fc.Features[0].Properties, fc.Features[0].Geometry.Type)

	df, err = featureCollectionToDataFrame(&fc, schema)
	if err != nil {
		return df, err
	}

	return df, nil
}

func createArrowSchemaFromProperties(properties map[string]any, geometryType string) *arrow.Schema {
	fields := make([]arrow.Field, 0, len(properties))

	for key, value := range properties {
		var fieldType arrow.DataType

		switch value.(type) {
		case float64:
			fieldType = arrow.PrimitiveTypes.Float64
		case string:
			fieldType = arrow.BinaryTypes.String
		case bool:
			fieldType = arrow.FixedWidthTypes.Boolean
		case int:
			fieldType = arrow.PrimitiveTypes.Int64
		case nil:
			fieldType = arrow.Null //Handle null values.
		default:
			fieldType = arrow.BinaryTypes.String // Default to string for unknown types.
			fmt.Printf(berrors.WarningUnknownPropertyType, key)
		}

		fields = append(fields, arrow.Field{Name: key, Type: fieldType, Nullable: true}) // All fields nullable for simplicity.
	}
	fields = append(fields, arrow.Field{Name: "geometry", Type: &gTypes.GeometryType{}, Nullable: true})

	return arrow.NewSchema(fields, nil)
}

func featureCollectionToDataFrame(fc *geojson.GeoJSONFeatureCollection, schema *arrow.Schema) (*gTypes.DataFrame, error) {
	df := gTypes.NewDataFrame(schema)

	pool := memory.NewGoAllocator()
	builders := make(map[string]array.Builder)

	// Initialize builders based on schema
	for _, field := range schema.Fields() {
		switch field.Type.ID() {
		case arrow.FLOAT64:
			builders[field.Name] = array.NewFloat64Builder(pool)
		case arrow.STRING:
			builders[field.Name] = array.NewStringBuilder(pool)
		case arrow.BOOL:
			builders[field.Name] = array.NewBooleanBuilder(pool)
		case arrow.INT64:
			builders[field.Name] = array.NewInt64Builder(pool)
		case arrow.NULL:
			builders[field.Name] = array.NewNullBuilder(pool)
		case arrow.EXTENSION:
			if _, ok := field.Type.(*gTypes.Point); ok {
				// Create a list builder for Point
				builders[field.Name] = array.NewListBuilder(pool, arrow.PrimitiveTypes.Float64)
			} else {
				builders[field.Name] = array.NewStringBuilder(pool) // Default to string for unknown types.
			}
		default:
			// Handle other types as needed
			builders[field.Name] = array.NewStringBuilder(pool) // Default to string for unknown types.
		}
	}

	for _, feature := range fc.Features {
		for key, value := range feature.Properties {
			builder, ok := builders[key]
			if !ok {
				return nil, fmt.Errorf("builder for key %s not found", key)
			}

			switch v := value.(type) {
			case float64:
				builder.(*array.Float64Builder).Append(v)
			case string:
				builder.(*array.StringBuilder).Append(v)
			case bool:
				builder.(*array.BooleanBuilder).Append(v)
			case int:
				builder.(*array.Int64Builder).Append(int64(v))
			case nil:
				builder.(*array.NullBuilder).AppendNull()
			default:
				builder.(*array.StringBuilder).Append(fmt.Sprintf("%v", v)) // Convert to string for unknown types.
			}
		}

		// Handle Geometry
		geoBuilder, ok := builders["geometry"]
		if !ok {
			return nil, fmt.Errorf("geometry builder not found")
		}

		switch feature.Geometry.Type {
		case "Point":
			coords := feature.Geometry.Coordinates
			if len(coords) != 2 {
				return nil, fmt.Errorf("invalid point coordinates")
			}

			coordsArray := [][][2]float64{coords[0].(float64), coords[1].(float64)}
			listBuilder := geoBuilder.(*array.ListBuilder)

			// Append the coordinates to the list builder
			float64Builder := array.NewFloat64Builder(pool)
			float64Builder.AppendValues(coordsArray[:], nil)
			listBuilder.Append(true)
			listBuilder.AppendValues(float64Builder.NewArray(), []bool{true})

		case "Polygon":
			// TODO: Implement Polygon handling
		case "LineString":
			// TODO: Implement LineString handling.
		default:
		}
	}

	// Build Arrow arrays from builders
	cols := make(map[string]gTypes.Series)
	for key, builder := range builders {
		cols[key] = gTypes.Series{Col: builder.NewArray()} // TODO: Fix this
	}

	// Create DataFrame
	for key, series := range cols {
		df.Series[key] = series
	}

	return df, nil
}
