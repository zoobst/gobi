package gbParquet

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	berrors "github.com/zoobst/gobi/bErrors"
	"github.com/zoobst/gobi/cmprssn"
	gTypes "github.com/zoobst/gobi/globalTypes"

	"github.com/apache/arrow/go/v18/arrow/memory"
	"github.com/apache/arrow/go/v18/parquet"
	"github.com/apache/arrow/go/v18/parquet/pqarrow"
)

// ReadParquet reads a Parquet file and converts it into a DataFrame structure
func ReadParquet(filePath string, compression string) (*gTypes.DataFrame, error) {
	f, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	err = handleCompression(compression, &f, false)
	if err != nil {
		log.Println("error decompressing file", err)
		return nil, err
	}

	table, err := pqarrow.ReadTable(context.Background(), bytes.NewReader(f), parquet.NewReaderProperties(nil),
		pqarrow.ArrowReadProperties{Parallel: true}, memory.DefaultAllocator)
	if err != nil {
		return nil, err
	}

	return gTypes.NewDataFrameFromTable(table), nil
}

func handleCompression(compression string, data *[]byte, compress bool) error {
	cType, err := parseCompressionType(compression)
	if err != nil {
		return err
	}

	switch c := cType.(type) {
	case *cmprssn.GzipCompression:
		if compress {
			c.Compress(data)
		} else {
			c.Decompress(data)
		}
	case *cmprssn.SnappyCompression:
		if compress {
			c.Compress(data)
		} else {
			c.Decompress(data)
		}
	case nil:
		return nil
	default:
		return berrors.ErrUnsupportedCompressionType
	}
	return nil
}

func parseCompressionType(compression string) (cmprssn.CompressionType, error) {
	c := strings.ToLower(compression)
	for s, t := range cmprssn.StringMap {
		if strings.Contains(c, fmt.Sprintf(".%s.", s)) {
			return t, nil
		} else if c == s {
			return t, nil
		} else {
			return nil, berrors.ErrUnsupportedCompressionType
		}
	}
	return nil, nil
}
