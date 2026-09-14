package buffer

import (
	"sync"
	"testing"
	"time"
)

func TestMergeSameWindow(t *testing.T) {
	var mu sync.Mutex
	var got []string
	agg := New(50*time.Millisecond, func(ev Event, types []string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev.Ident+"|"+join(types))
	})
	agg.Add(Event{Ident: "host01", Type: "nic", Window: 2982192, TriggerTime: 1789315613})
	agg.Add(Event{Ident: "host01", Type: "mem", Window: 2982192, TriggerTime: 1789315615})
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("want 1 merged callback, got %v", got)
	}
	if got[0] != "host01|nic,mem" && got[0] != "host01|mem,nic" {
		t.Fatalf("want merged types nic+mem, got %s", got[0])
	}
}

func TestNoMergeAcrossWindows(t *testing.T) {
	var mu sync.Mutex
	var got []string
	agg := New(50*time.Millisecond, func(ev Event, types []string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev.Ident+"|"+join(types))
	})
	agg.Add(Event{Ident: "host01", Type: "nic", Window: 2982192})
	agg.Add(Event{Ident: "host01", Type: "mem", Window: 2982193})
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("want 2 separate callbacks, got %v", got)
	}
}

func TestDrainFlushesPending(t *testing.T) {
	var mu sync.Mutex
	var got []string
	agg := New(time.Hour, func(ev Event, types []string) { // 定时器远未到期
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev.Ident+"|"+join(types))
	})
	agg.Add(Event{Ident: "host01", Type: "nic", Window: 2982192})
	agg.Add(Event{Ident: "host01", Type: "mem", Window: 2982192})
	agg.Drain()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("drain should flush pending entries once, got %v", got)
	}
	if got[0] != "host01|nic,mem" && got[0] != "host01|mem,nic" {
		t.Fatalf("drain should merge types, got %s", got[0])
	}
}

func join(ss []string) string {
	s := ""
	for i, v := range ss {
		if i > 0 {
			s += ","
		}
		s += v
	}
	return s
}
