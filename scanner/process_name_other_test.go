//go:build !linux

package scanner

import (
	"context"
	"testing"
)

func TestClassificationNameKeepsReportedNameOutsideLinux(t *testing.T) {
	// A nil process would panic if the executable were read.
	for _, name := range []string{"MainThread", "node", "pi"} {
		if got := classificationName(context.Background(), nil, name); got != name {
			t.Errorf("classificationName(%q) = %q, want the reported name", name, got)
		}
	}
}
