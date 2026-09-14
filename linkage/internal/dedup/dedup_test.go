package dedup

import (
	"testing"
	"time"
)

func TestMemoryDedup(t *testing.T) {
	d := NewMemory(30 * time.Second)
	if !d.Acquire("host01", "nic", 100) {
		t.Fatal("first acquire should succeed")
	}
	if d.Acquire("host01", "nic", 100) {
		t.Fatal("duplicate should be rejected")
	}
	if !d.Acquire("host01", "mem", 100) {
		t.Fatal("different type same window should succeed")
	}
	if !d.Acquire("host01", "nic", 101) {
		t.Fatal("different window should succeed")
	}
}
