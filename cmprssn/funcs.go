package cmprssn

import (
	"bytes"
	"compress/gzip"
	"io"

	"github.com/golang/snappy"
)

// Compress compresses data using Gzip.
func (g *GzipCompression) Compress(data *[]byte) error {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	_, err := writer.Write(*data)
	if err != nil {
		return err
	}
	err = writer.Close()
	if err != nil {
		return err
	}
	*data = buf.Bytes()
	return nil
}

// Decompress decompresses data using Gzip.
func (g *GzipCompression) Decompress(data *[]byte) error {
	reader, err := gzip.NewReader(bytes.NewReader(*data))
	if err != nil {
		return err
	}
	defer reader.Close()
	var buf bytes.Buffer
	_, err = io.Copy(&buf, reader)
	if err != nil {
		return err
	}
	*data = buf.Bytes()
	return nil
}

func (s *SnappyCompression) Compress(data *[]byte) error {
	*data = snappy.Encode(nil, *data) // Compress data with Snappy
	return nil
}

// Decompress decompresses data using Snappy.
func (s *SnappyCompression) Decompress(data *[]byte) error {
	decodedData, err := snappy.Decode(nil, *data) // Decompress data with Snappy
	if err != nil {
		return err
	}
	*data = decodedData
	return nil
}
