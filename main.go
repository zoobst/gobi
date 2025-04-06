package main

import (
	"fmt"
	"log"

	"github.com/zoobst/gobi/gbParquet"
)

func main() {

	df, err := gbParquet.ReadParquet("testData/titanic_test.parquet")
	if err != nil {
		log.Fatal(err)
	}

	for _, ser := range df.Series {
		log.Println(ser.Name)
	}

	fmt.Println(df.Head(10))
	fmt.Println(df.Tail(10))
}
