package repolink

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// The public-repository memory holds at most PublicCheckEntries entries and
// drops the oldest first (D84).
func TestPublicCacheIsBounded(t *testing.T) {
	t.Parallel()
	f := NewFiller("", &http.Client{})
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	for i := range PublicCheckEntries + 100 {
		f.remember(fmt.Sprintf("o/r%d", i), publicEntry{public: true, at: now.Add(time.Duration(i) * time.Second)}, now.Add(time.Duration(i)*time.Second))
	}
	if len(f.public) != PublicCheckEntries {
		t.Fatalf("%d entries, want %d", len(f.public), PublicCheckEntries)
	}
	for i := range 100 {
		if _, ok := f.public[fmt.Sprintf("o/r%d", i)]; ok {
			t.Fatalf("o/r%d, among the oldest 100, is still cached", i)
		}
	}
	if _, ok := f.public[fmt.Sprintf("o/r%d", PublicCheckEntries+99)]; !ok {
		t.Error("the newest entry was evicted")
	}
}
