package geometry

import (
	"math"
)

func LLToMercator(lng, lat float64) (x, y float64) {
	x = lng * MERCATOR_TRANSFORM_VAL / 180
	y = math.Log(math.Tan((90+lat)*math.Pi/360)) / (math.Pi / 180)
	y = y * MERCATOR_TRANSFORM_VAL / 180
	return x, y
}

func MercatorToLL(x, y float64) (lng, lat float64) {
	lng = x / MERCATOR_TRANSFORM_VAL * 180
	lat = y / MERCATOR_TRANSFORM_VAL * 180
	lat = 180 / math.Pi * (2*math.Atan(math.Exp(lat*math.Pi/180)) - math.Pi/2)
	return lng, lat
}
