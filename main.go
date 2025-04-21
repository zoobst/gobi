package main

import (
	"log"

	gbcsv "github.com/zoobst/gobi/gbCsv"
	"github.com/zoobst/gobi/gbParquet"
	gTypes "github.com/zoobst/gobi/globalTypes"
	"github.com/zoobst/gobi/writers"
)

type DataFrame map[string]*gTypes.Series

// type DataFrame struct {
// 	*gTypes.DataFrame
// }

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

func ReadCSV(path string, options gbcsv.CsvReadOptions) (*DataFrame, error) {
	df, err := gbcsv.ReadCsv(path, options)
	if err != nil {
		return nil, err
	}
	return &DataFrame{df}, nil
}

func ReadCSVFromType[T struct{}](t T, path string, options gbcsv.CsvReadOptions) (*DataFrame, error) {
	df, err := gbcsv.ReadFromGeneric(t, path, gbcsv.CsvReadOptions{})
	if err != nil {
		return nil, err
	}

	return &DataFrame{df}, nil
}

func main() {
	// df2, err := ReadCSV(tests.TestCSVTypes{}, "testData/titanic_test.csv", gbcsv.CsvReadOptions{})
	df2, err := ReadCSVFromType("testData/titanic_test.csv", gbcsv.CsvReadOptions{})
	if err != nil {
		log.Fatal(err)
	}
	log.Println(df2.Head(10))
	log.Println(df2.Iloc(9))
	log.Println(df2.Series[0].Iloc(9))
}
