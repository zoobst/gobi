package cmprssn

type CompressionType interface {
	Compress(data *[]byte) (*[]byte, error)   // Compress the data
	Decompress(data *[]byte) (*[]byte, error) // Decompress the data
}

type GzipCompression struct{}

type SnappyCompression struct{}

type None struct{}
