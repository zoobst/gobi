//go:build !js && !wasip1

package gobi

import (
	"testing"

	"github.com/zoobst/gobi/internal/fmacheck"
)

// TestNoFusedMultiplyAdd keeps this package's floating-point results
// the same on every platform: see fmacheck.
func TestNoFusedMultiplyAdd(t *testing.T) { fmacheck.Check(t) }
