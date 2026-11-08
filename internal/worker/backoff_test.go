package worker

import (
	"testing"
	"time"
)

func TestBackoffResetAndCap(t *testing.T) {
	b := backoffInitial
	for i := 0; i < 10; i++ {
		b = growBackoff(b)
	}
	if b != backoffMax {
		t.Fatalf("cap: got %v want %v", b, backoffMax)
	}
	if got := resetBackoff(b, time.Second); got != b {
		t.Fatalf("short connection must not reset: %v", got)
	}
	if got := resetBackoff(b, backoffHealthyUp); got != backoffInitial {
		t.Fatalf("healthy connection must reset: %v", got)
	}
	if got := growBackoff(backoffInitial); got != 2*time.Second {
		t.Fatalf("grow: %v", got)
	}
}
