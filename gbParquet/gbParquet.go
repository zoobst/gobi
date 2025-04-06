package gbParquet

import (
	"context"
	"os"

	gTypes "github.com/zoobst/gobi/globalTypes"

	"github.com/apache/arrow/go/v18/arrow/memory"
	"github.com/apache/arrow/go/v18/parquet"
	"github.com/apache/arrow/go/v18/parquet/pqarrow"
)

// ReadParquet reads a Parquet file and converts it into a DataFrame structure
func ReadParquet(filePath string) (*gTypes.DataFrame, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	table, err := pqarrow.ReadTable(context.Background(), f, parquet.NewReaderProperties(nil),
		pqarrow.ArrowReadProperties{Parallel: true}, memory.DefaultAllocator)
	if err != nil {
		return nil, err
	}

	return gTypes.NewDataFrameFromTable(table), nil
}
