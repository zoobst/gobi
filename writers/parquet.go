package writers

import (
	"os"

	"github.com/apache/arrow/go/v18/parquet"
	"github.com/apache/arrow/go/v18/parquet/pqarrow"
	gTypes "github.com/zoobst/gobi/globalTypes"
)

func WriteParquetToFile(df *gTypes.DataFrame, path, compression string) error {
	outFile, err := os.Create(path)
	if err != nil {
		return err
	}
	defer outFile.Close()

	writer, err := pqarrow.NewFileWriter(df.Schema(), outFile, parquet.NewWriterProperties(), pqarrow.NewArrowWriterProperties(pqarrow.WithDeprecatedInt96Timestamps(false)))
	if err != nil {
		return err
	}

	err = writer.WriteTable(*df, 8162)
	if err != nil {
		return err
	}
	return nil
}
