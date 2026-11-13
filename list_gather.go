package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/bitutil"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// List operations that pick elements out of lists (Explode, ListGet,
// ListSlice) collect the positions they want in each chunk's values
// array and copy them in one compute.Take. That handles every element
// type (structs, nested lists, maps, dates, decimals, ...) and keeps
// the exact element type, where copying element by element through a
// builder only ever covered the simple types.

// elemGather collects, chunk by chunk, positions into each chunk's list
// values; -1 means a null element.
type elemGather struct {
	pool   memory.Allocator
	pieces []arrow.Array
}

// add copies values[idx] (−1 = null) as the next piece.
func (g *elemGather) add(values arrow.Array, idx []int) error {
	if len(idx) == 0 {
		return nil
	}
	p, err := takeCompute(g.pool, values, idx, true)
	if err != nil {
		return err
	}
	g.pieces = append(g.pieces, p)
	return nil
}

// finish returns every piece as one array of type dt; the caller owns it.
func (g *elemGather) finish(dt arrow.DataType) (arrow.Array, error) {
	pieces := g.pieces
	g.pieces = nil
	return concatOwned(g.pool, dt, pieces)
}

// concatOwned joins pieces into one array of type dt (empty when there
// are none) and releases them; the caller owns the result.
func concatOwned(pool memory.Allocator, dt arrow.DataType, pieces []arrow.Array) (arrow.Array, error) {
	defer func() {
		for _, p := range pieces {
			p.Release()
		}
	}()
	switch len(pieces) {
	case 0:
		return array.MakeArrayOfNull(pool, dt, 0), nil
	case 1:
		pieces[0].Retain()
		return pieces[0], nil
	}
	out, err := array.Concatenate(pieces, pool)
	if err != nil {
		return nil, fmt.Errorf("%w: concatenate %s: %v", ErrColumnTypeMismatch, dt, err)
	}
	return out, nil
}

func (g *elemGather) release() {
	for _, p := range g.pieces {
		p.Release()
	}
	g.pieces = nil
}

// listChunks returns s's chunks, which must all be *array.List.
func listChunks(s Series, what string) ([]*array.List, error) {
	chunks := s.col.Data().Chunks()
	out := make([]*array.List, len(chunks))
	for i, c := range chunks {
		la, ok := c.(*array.List)
		if !ok {
			return nil, fmt.Errorf("%w: %s: list chunk not *array.List (%T)", ErrColumnTypeMismatch, what, c)
		}
		out[i] = la
	}
	return out, nil
}

// newListArray builds a list of type lt over values from offsets (one
// more than the row count) and a validity mask (nil = no nulls).
func newListArray(lt *arrow.ListType, offsets []int32, valid []bool, values arrow.Array) arrow.Array {
	n := len(offsets) - 1
	buffers := []*memory.Buffer{nil, memory.NewBufferBytes(arrow.Int32Traits.CastToBytes(offsets))}
	nulls := 0
	if valid != nil {
		bitmap := make([]byte, bitutil.BytesForBits(int64(n)))
		for i, ok := range valid {
			if ok {
				bitutil.SetBit(bitmap, i)
			} else {
				nulls++
			}
		}
		if nulls > 0 {
			buffers[0] = memory.NewBufferBytes(bitmap)
		}
	}
	data := array.NewData(lt, n, buffers, []arrow.ArrayData{values.Data()}, nulls, 0)
	defer data.Release()
	return array.MakeFromData(data)
}
