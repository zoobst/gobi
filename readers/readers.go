package readers

import "io"

type Reader interface {
	io.Reader
	handleCompression()
	parseCompressionType()
}
