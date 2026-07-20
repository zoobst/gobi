package gobi

import (
	"strconv"

	"github.com/apache/arrow-go/v18/arrow"
)

// Schema metadata keys used by gobi to tag geometry columns. This follows the
// GeoParquet convention closely enough that files written by gobi should be
// readable by other GeoParquet-aware tools for primitive geometry types.
const (
	MetaGeometryType = "gobi:geometry_type"
	MetaGeometryCRS  = "gobi:crs_epsg"
)

// isGeometryField reports whether f is tagged as a geometry column.
func isGeometryField(f arrow.Field) bool {
	if f.Type.ID() != arrow.BINARY {
		return false
	}
	_, ok := f.Metadata.GetValue(MetaGeometryType)
	return ok
}

// GeometryField returns a schema field tagged as a WKB geometry column.
// Pass epsg=0 to leave the CRS unset.
func GeometryField(name string, epsg int32) arrow.Field {
	md := arrow.NewMetadata(
		[]string{MetaGeometryType, MetaGeometryCRS},
		[]string{"WKB", strconv.FormatInt(int64(epsg), 10)},
	)
	return arrow.Field{
		Name:     name,
		Type:     arrow.BinaryTypes.Binary,
		Nullable: true,
		Metadata: md,
	}
}

// geometryCRSFromField reads the EPSG code stored in a geometry field's
// schema metadata. Returns 0 if unset or unparseable.
func geometryCRSFromField(f arrow.Field) int32 {
	s, ok := f.Metadata.GetValue(MetaGeometryCRS)
	if !ok || s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0
	}
	return int32(v)
}
