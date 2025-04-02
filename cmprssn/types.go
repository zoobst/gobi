package cmprssn

type CompressionType interface {
	Compress(data *[]byte) error   // Compress the data
	Decompress(data *[]byte) error // Decompress the data
}

type GzipCompression struct{}

type SnappyCompression struct{}
