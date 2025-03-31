package cmprssn

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
)

// Compress compresses data using Gzip.
func (g *GzipCompression) Compress(data *[]byte) (*[]byte, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	_, err := writer.Write(*data)
	if err != nil {
		return nil, err
	}
	err = writer.Close()
	if err != nil {
		return nil, err
	}
	outData := buf.Bytes()
	return &outData, nil
}

// Decompress decompresses data using Gzip.
func (g *GzipCompression) Decompress(data *[]byte) (*[]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(*data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var buf bytes.Buffer
	_, err = io.Copy(&buf, reader)
	if err != nil {
		return nil, err
	}
	outData := buf.Bytes()
	return &outData, nil
}

func (s *SnappyCompression) Compress(data *[]byte) (*[]byte, error) {
	// Implement Snappy compression logic here
	return nil, errors.New("not implemented")
}

func (s *SnappyCompression) Decompress(data *[]byte) (*[]byte, error) {
	// Implement Snappy decompression logic here
	return nil, errors.New("not implemented")
}

// Compress compresses data using Gzip.
func (n *None) Compress(data *[]byte) (*[]byte, error) {
	return data, nil
}

func (n *None) Decompress(data *[]byte) (*[]byte, error) {
	return data, nil
}
