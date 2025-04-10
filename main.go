package main

import (
	"fmt"
	"log"

	"github.com/zoobst/gobi/gbParquet"
	gTypes "github.com/zoobst/gobi/globalTypes"
	"github.com/zoobst/gobi/writers"
)

type DataFrame struct {
	df *gTypes.DataFrame
}

func (df *DataFrame) ToParquet(outPath, compression string) (err error) {
	return writers.WriteParquetToFile(df.df, outPath, compression)
}

func ReadParquet(path string, compression string) (*DataFrame, error) {
	if df, err := gbParquet.ReadParquet(path, compression); err == nil {
		return &DataFrame{df: df}, err
	} else {
		return nil, err
	}
}

func main() {
	df, err := ReadParquet("testData/titantic_test_out.snappy.parquet", "snappy")
	if err != nil {
		log.Fatal(err)
	}

	for _, ser := range df.df.Series {
		log.Println(ser.Name)
	}

	fmt.Println(df.df.Head(10))
	fmt.Println(df.df.Tail(10))

	err = df.ToParquet("testData/titantic_test_out.gz.parquet", "gz")
	if err != nil {
		log.Fatal(err)
	}
}
