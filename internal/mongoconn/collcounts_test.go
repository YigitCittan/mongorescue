package mongoconn

import "testing"

// TestCountsAsView checks that only views go uncounted: a time-series
// collection holds measurements, and the swap of an in-place point-in-time
// restore must compare its copy's count like any collection's.
func TestCountsAsView(t *testing.T) {
	for typ, want := range map[string]bool{"view": true, "collection": false, "timeseries": false, "": false} {
		if got := countsAsView(typ); got != want {
			t.Errorf("countsAsView(%q) = %v, want %v", typ, got, want)
		}
	}
}
