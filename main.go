package main

import (
	"fmt"
	"log"

	"github.com/zoobst/gobi/gbParquet"
	gTypes "github.com/zoobst/gobi/globalTypes"
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

func main() {
	df, err := ReadParquet("testData/titantic_test_out.snappy.parquet", "snappy")
	if err != nil {
		log.Fatal(err)
	}

	for _, ser := range df.Series {
		log.Println(ser.Name)
	}

	fmt.Println(df.Head(10))
	fmt.Println(df.Tail(10))

	err = df.ToParquet("testData/titantic_test_out.gz.parquet", "gz")
	if err != nil {
		log.Fatal(err)
	}
}
