package bbr

import "testing"

func TestRingBufferInitIsLazyAndResetsState(t *testing.T) {
	var r RingBuffer[int]
	r.Init(256)
	if cap(r.ring) != 0 || !r.Empty() || r.Len() != 0 {
		t.Fatalf("lazy Init state: cap=%d empty=%v len=%d", cap(r.ring), r.Empty(), r.Len())
	}
	r.PushBack(1)
	if cap(r.ring) != initialRingSize || r.Len() != 1 || *r.Front() != 1 {
		t.Fatalf("first growth: cap=%d len=%d front=%d", cap(r.ring), r.Len(), *r.Front())
	}
	r.Init(256)
	if cap(r.ring) != 0 || !r.Empty() {
		t.Fatalf("re-Init did not reset the ring: cap=%d empty=%v", cap(r.ring), r.Empty())
	}
}

func TestRingBufferLazyGrowthPreservesFIFO(t *testing.T) {
	var r RingBuffer[int]
	r.Init(256)
	for i := 0; i < 1024; i++ {
		r.PushBack(i)
	}
	if cap(r.ring) < 1024 || r.Len() != 1024 {
		t.Fatalf("busy ring did not grow: cap=%d len=%d", cap(r.ring), r.Len())
	}
	for i := 0; i < 1024; i++ {
		if got := r.PopFront(); got != i {
			t.Fatalf("PopFront #%d = %d", i, got)
		}
	}
	if !r.Empty() {
		t.Fatal("ring not empty after draining")
	}
}
