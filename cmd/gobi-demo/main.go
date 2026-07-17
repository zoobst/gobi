// gobi-demo is a small CLI that exercises gobi's CSV → Parquet flow. Given
// an input CSV of the form:
//
//	name,population,geometry
//	New York,8804190,POINT (-74.0060 40.7128)
//	...
//
// it prints the frame's shape and geometry column, then writes a compressed
// Parquet copy alongside the input.
package main

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/zoobst/gobi/csvio"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/parquetio"
)

type city struct {
	Name       string `csv:"name"`
	Population int64  `csv:"population"`
	Geom       string `csv:"geometry" geom:"true"`
}

func main() {
	log.SetFlags(0)

	var (
		inputPath  = flag.String("in", "testdata/cities.csv", "input CSV path")
		outputPath = flag.String("out", "", "output Parquet path (default: <in>.parquet)")
		codecName  = flag.String("codec", "snappy", "Parquet compression codec")
	)
	flag.Parse()

	codec, err := parquetio.ParseCodec(*codecName)
	if err != nil {
		log.Fatalf("invalid codec: %v", err)
	}

	df, err := csvio.ReadFile[city](*inputPath, &csvio.Options{CRSHint: 4326})
	if err != nil {
		log.Fatalf("read csv: %v", err)
	}
	defer df.Release()

	rows, cols := df.Shape()
	fmt.Printf("Read %d rows x %d columns from %s\n", rows, cols, *inputPath)
	fmt.Printf("Columns: %s\n", strings.Join(df.ColumnNames(), ", "))

	for i := range rows {
		g, err := df.Geometry("geometry", i)
		if err != nil {
			log.Fatalf("row %d geometry: %v", i, err)
		}
		if p, ok := g.(geometry.Point); ok {
			fmt.Printf("  row %d @ (%.4f, %.4f)\n", i, p.X, p.Y)
		}
	}

	out := *outputPath
	if out == "" {
		out = strings.TrimSuffix(*inputPath, filepath.Ext(*inputPath)) + ".parquet"
	}
	if err := parquetio.WriteFile(df, out, codec); err != nil {
		log.Fatalf("write parquet: %v", err)
	}
	fmt.Printf("Wrote %s (%s)\n", out, codec)
}
