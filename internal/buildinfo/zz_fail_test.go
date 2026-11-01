package buildinfo

import "testing"

// Deliberately failing test to prove CI OK turns red (angelia#41). Never merge.
func TestDeliberateFailure(t *testing.T) { t.Fatal("deliberate failure for CI OK proof") }
