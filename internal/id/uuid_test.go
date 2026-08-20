package id

import (
	"regexp"
	"testing"
	"time"
)

var canonicalUUIDv7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewShapeAndUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		v := New()
		if !canonicalUUIDv7.MatchString(v) {
			t.Fatalf("not a canonical UUIDv7: %q", v)
		}
		if _, dup := seen[v]; dup {
			t.Fatalf("duplicate id generated: %q", v)
		}
		seen[v] = struct{}{}
	}
}

func TestNewIsTimeSortable(t *testing.T) {
	earlier := newAt(time.UnixMilli(1_000_000))
	later := newAt(time.UnixMilli(2_000_000))
	if earlier >= later {
		t.Fatalf("expected earlier id %q < later id %q", earlier, later)
	}
}
