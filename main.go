package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/apache/arrow/go/v18/arrow/csv"
	gbcsv "github.com/zoobst/gobi/gbCsv"
	"github.com/zoobst/gobi/gbParquet"
	gTypes "github.com/zoobst/gobi/globalTypes"
	"github.com/zoobst/gobi/writers"
)

type DataFrame struct {
	*gTypes.DataFrame
}

type Geometry interface {
	gTypes.Geometry
}

func ReadParquet(path string, compression string) (*DataFrame, error) {
	if df, err := gbParquet.ReadParquet(path, compression); err == nil {
		return &DataFrame{df}, nil
	} else {
		return nil, err
	}
}

func (df *DataFrame) ToParquet(outPath, compression string) (err error) {
	return writers.WriteParquetToFile(df, outPath, compression)
}

func ReadCSVFromType[T any](t T, path string, options gbcsv.CsvReadOptions) (*DataFrame, error) {
	df, err := gbcsv.ReadFromGeneric[T](t, path, gbcsv.CsvReadOptions{})
	if err != nil {
		return nil, err
	}

	return &DataFrame{df}, nil
}

func ReadCSVFromTypeUsingArrow[T any](t T, path string, options ...csv.Option) (*DataFrame, error) {
	df, err := gbcsv.ReadCSVUsingArrow(t, path, options...)
	if err != nil {
		return nil, err
	}

	return &DataFrame{df}, nil
}

func main() {
	start := time.Now()

	cpuFile, err := os.Create("cpu.prof")
	if err != nil {
		log.Fatal("could not create CPU profile: ", err)
	}
	defer cpuFile.Close()

	pprof.StartCPUProfile(cpuFile)
	defer pprof.StopCPUProfile()

	f, err := os.Create("mem.prof")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	defer pprof.WriteHeapProfile(f)

	elapsed := time.Since(start)
	fmt.Printf("Elapsed time: %s\n", elapsed)

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("Total memory allocated: %v MiB\n", m.TotalAlloc/(1024*1024))
}
