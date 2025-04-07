package readers

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	berrors "github.com/zoobst/gobi/bErrors"
	"github.com/zoobst/gobi/cmprssn"
)

type ParquetReader struct {
	io.Reader
	data []byte

	compressionType cmprssn.CompressionType
}

func NewParquetReader(filePath string, compression string) (*ParquetReader, error) {
	f, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	pr := ParquetReader{
		data: f,
	}

	err = pr.parseCompressionType(compression)
	if err != nil {
		return &pr, err
	}

	err = pr.handleCompression()
	if err != nil {
		log.Println("error decompressing file", err)
		return nil, err
	}

	return &pr, nil
}

func (pr ParquetReader) handleCompression() error {
	switch c := pr.compressionType.(type) {
	case nil:
		return nil
	default:
		return c.Decompress(&pr.data)
	}
}

func (pr *ParquetReader) parseCompressionType(compression string) error {
	c := strings.ToLower(compression)
	if c == "" {
		pr.compressionType = nil
		return nil
	}
	for s := range cmprssn.StringMap {
		if strings.Contains(c, fmt.Sprintf(".%s.", s)) {
			pr.compressionType = cmprssn.StringMap[s]
			return nil
		} else if compression == s {
			pr.compressionType = cmprssn.StringMap[s]
			return nil
		} else {
			return berrors.ErrUnsupportedCompressionType
		}
	}
	return nil
}

func (pr ParquetReader) Read(b []byte) (n int, err error) {
	if b == nil {

	}
}
