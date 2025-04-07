package cmprssn

const SnappyString string = "snappy"
const GzipString string = "gzip"
const GzString string = "gz"

var StringMap = map[string]CompressionType{
	SnappyString: &SnappyCompression{},
	GzipString:   &GzipCompression{},
	GzString:     &GzipCompression{},
}

type CompressionType interface {
	Compress(data *[]byte) error   // Compress the data
	Decompress(data *[]byte) error // Decompress the data
}

type GzipCompression struct{}

type SnappyCompression struct{}
