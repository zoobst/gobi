package csvio_test

import (
	"strings"
	"testing"

	"github.com/zoobst/gobi/csvio"
	"github.com/zoobst/gobi/geometry"
)

type city struct {
	Name       string `csv:"name"`
	Population int64  `csv:"population"`
	Geom       string `csv:"geometry" geom:"true"`
}

const citiesCSV = `name,population,geometry
New York,8804190,POINT (-74.0060 40.7128)
Los Angeles,3898747,POINT (-118.2437 34.0522)
Chicago,2746388,POINT (-87.6298 41.8781)
`

func TestRead_Cities(t *testing.T) {
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.Options{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	rows, cols := df.Shape()
	if rows != 3 || cols != 3 {
		t.Fatalf("shape got (%d, %d) want (3, 3)", rows, cols)
	}
	names := df.ColumnNames()
	if names[0] != "name" || names[1] != "population" || names[2] != "geometry" {
		t.Fatalf("names: %v", names)
	}

	g, err := df.Geometry("geometry", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := g.(geometry.Point)
	if !ok {
		t.Fatalf("expected Point, got %T", g)
	}
	if p.X > -74 || p.X < -74.01 {
		t.Fatalf("X = %v", p.X)
	}
}

func TestReadFile_Cities(t *testing.T) {
	df, err := csvio.ReadFile[city]("../testdata/cities.csv", &csvio.Options{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := df.Shape()
	if rows != 5 {
		t.Fatalf("rows = %d, want 5", rows)
	}
}
