package main

import (
	"fmt"
	"log"

	"github.com/zoobst/gobi/cmprssn"
	"github.com/zoobst/gobi/gbParquet"
	gTypes "github.com/zoobst/gobi/globalTypes"
)

func ReadParquet(path string, compression cmprssn.CompressionType) (*gTypes.DataFrame, error) {
	return gbParquet.ReadParquet(path, compression)
}

func main() {
	df, err := ReadParquet("testData/titanic_test.gz.parquet", &cmprssn.GzipCompression{})
	if err != nil {
		log.Fatal(err)
	}

	for _, ser := range df.Series {
		log.Println(ser.Name)
	}

	fmt.Println(df.Head(10))
	fmt.Println(df.Tail(10))
}
