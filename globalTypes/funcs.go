package globalTypes

import (
	"fmt"
)

func (s String) String() string {
	return s.val
}

func (i Int) String() string {
	return fmt.Sprintf("%d", i.val)
}

func (f Float) String() string {
	return fmt.Sprintf("%f", f.val)
}

func (t DateTime) String() string {
	return t.val.String()
}

func (b Bool) String() string {
	return fmt.Sprintf("%v", b.val)
}

func maxY[p *[]Point](points *[]Point) (hVal float64) {
	hVal = -10_000
	if len(*points) == 0 {
		return 0.0
	}
	for _, point := range *points {
		if point.Y > hVal {
			hVal = point.Y
		}
	}
	return hVal
}

func maxX[p *[]Point](points *[]Point) (hVal float64) {
	hVal = -10_000
	if len(*points) == 0 {
		return 0.0
	}
	for _, point := range *points {
		if point.X > hVal {
			hVal = point.X
		}
	}
	return hVal
}

func minY[p *[]Point](points *[]Point) (lVal float64) {
	lVal = 10_000
	if len(*points) == 0 {
		return 0.0
	}
	for _, point := range *points {
		if point.Y < lVal {
			lVal = point.Y
		}
	}
	return lVal
}

func minX[p *[]Point](points *[]Point) (lVal float64) {
	lVal = 1_000_000
	if len(*points) == 0 {
		return 0.0
	}
	for _, point := range *points {
		if point.X < lVal {
			lVal = point.X
		}
	}
	return lVal
}

func NewHashSet() *HashSet {
	return &HashSet{
		data: make(map[any]struct{}),
	}
}

func (hs *HashSet) Add(value any) {
	hs.data[value] = struct{}{}
}

func (hs *HashSet) Remove(value any) {
	delete(hs.data, value)
}

func (hs *HashSet) Contains(value any) bool {
	_, exists := hs.data[value]
	return exists
}

func (hs *HashSet) Len() int {
	return len(hs.data)
}

func (hs *HashSet) Clear() {
	hs.data = make(map[any]struct{})
}

func (hs *HashSet) List() []any {
	result := make([]any, 0, len(hs.data))
	for key := range hs.data {
		result = append(result, key)
	}
	return result
}
