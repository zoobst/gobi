package geometry

import (
	"encoding/hex"
	"encoding/json"
	"errors"
)

func (gc GeometryCollection) Name() string {
	return "GeometryCollection"
}

func (gc GeometryCollection) Type() string {
	return "GeometryCollection"
}

func (gc GeometryCollection) CRS() *CRS {
	return gc.Geometries[0].CRS()
}

func (gc GeometryCollection) EstimateUTMCRS() CRS {
	crs := EstimateUTMCRS(gc.Geometries[0])
	return CRSbyEPSG[crs]
}

func (gc *GeometryCollection) ToCRS(epsg int32) error {
	newGC := GeometryCollection{}
	for _, geom := range gc.Geometries {
		err := ToCRS(&geom, epsg)
		if err != nil {
			return err
		}
		newGC.Geometries = append(newGC.Geometries, geom)
	}
	gc.Geometries = newGC.Geometries
	return nil
}

func (gc GeometryCollection) Coords() (coordArray [][2]float64) {
	for _, geometry := range gc.Geometries {
		coordArray = append(coordArray, geometry.Coords()...)
	}
	return
}

func (gc GeometryCollection) Bounds() (b Box) {
	b = gc.Geometries[0].Bounds()
	for i := range len(gc.Geometries) - 1 {
		b = b.maxBox(gc.Geometries[i+1].Bounds())
	}
	return
}

func (gc GeometryCollection) Len() int {
	return len(gc.Geometries)
}

func (gc GeometryCollection) Equal(other Geometry) bool {
	switch t := other.(type) {
	case GeometryCollection:
		for i, geom := range gc.Geometries {
			if !Equal(geom, t.Geometries[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (gc GeometryCollection) WKT() string {
	strList := "GEOMETRYCOLLECTION("
	for _, geom := range gc.Geometries {
		strList = strList + "," + geom.WKT()
	}
	strList = strList[:len(strList)-1]
	return strList + ")"
}

func (gc GeometryCollection) WKB() (output []byte) {
	for _, geo := range gc.Geometries {
		output = append(output, geo.WKB()...)
	}
	return
}

func (gc GeometryCollection) WKBHex() (string, error) {
	var byteArray []byte
	for _, geo := range gc.Geometries {
		byteArray = append(byteArray, geo.WKB()...)
	}
	return hex.EncodeToString(byteArray), nil
}

func (gc GeometryCollection) FromWKB(data []byte) (GeometryCollection, error) {
	return GeometryCollection{}, nil
}

func (gc GeometryCollection) MaxX() (maxX float64) {
	for _, geom := range gc.Geometries {
		maxX = max(geom.MaxX(), maxX)
	}
	return
}

func (gc GeometryCollection) MaxY() (maxY float64) {
	for _, geom := range gc.Geometries {
		maxY = max(geom.MaxY(), maxY)
	}
	return
}

func (gc GeometryCollection) MinX() (minX float64) {
	for _, geom := range gc.Geometries {
		minX = min(geom.MinX(), minX)
	}
	return
}

func (gc GeometryCollection) MinY() (minY float64) {
	for _, geom := range gc.Geometries {
		minY = min(geom.MinY(), minY)
	}
	return
}

func (gc GeometryCollection) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type       string     `json:"type"`
		Geometries []Geometry `json:"geometries"`
	}{
		Type:       gc.Name(),
		Geometries: gc.Geometries,
	})
}

func (gc GeometryCollection) UnmarshalJSON(data []byte) error {
	temp := struct {
		Type       string     `json:"type"`
		Geometries []Geometry `json:"geometries"`
	}{}
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	if temp.Type != gc.Name() {
		return errors.New("invalid geometry type for Polygon")
	}

	gc.Geometries = temp.Geometries

	return nil
}
