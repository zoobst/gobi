package main

import (
	"log"
	"os"

	"github.com/zoobst/gobi/gbParquet"
	gTypes "github.com/zoobst/gobi/globalTypes"
	"github.com/zoobst/gobi/readers"
	"github.com/zoobst/gobi/tests"
	"github.com/zoobst/gobi/writers"
)

type DataFrame struct {
	*gTypes.DataFrame
}

func ReadParquet(path string, compression string) (*DataFrame, error) {
	if df, err := gbParquet.ReadParquet(path, compression); err == nil {
		return &DataFrame{df}, err
	} else {
		return nil, err
	}
}

func (df *DataFrame) ToParquet(outPath, compression string) (err error) {
	return writers.WriteParquetToFile(df, outPath, compression)
}

func ReadCSV(path string, compression string) (string, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	testReader := tests.TestCSVTypes{}

	if reader, err := readers.NewGenericCSVReader(testReader, &file); err != nil {
		return "", err
	} else {
		return reader.Schema.String(), nil
	}

}

func main() {
	df, err := ReadCSV("testData/titanic_test.csv", "")
	if err != nil {
		log.Fatal(err)
	}
	log.Println(df)
}
