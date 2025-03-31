package gbcsv

import "time"

var TimeFormatsList = [24]string{
	time.RFC1123Z,
	time.RFC1123,
	time.RFC3339,
	time.RFC3339Nano,
	time.RFC822,
	time.RFC822Z,
	time.RFC850,
	time.Kitchen,
	time.DateTime,
	time.DateOnly,
	time.Stamp,
	time.StampMicro,
	time.StampMilli,
	time.StampNano,
	time.RubyDate,
	time.TimeOnly,
	time.UnixDate,
	time.ANSIC,
}
