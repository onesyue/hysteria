package bbr

import (
	"math/rand"
	"runtime"
	"testing"

	"github.com/apernet/quic-go/congestion"
)

// eagerRingBuffer 是打补丁**之前**的 RingBuffer，逐字抄下来当对拍基准。
// 有它，"惰性化改坏了语义" 就不再靠肉眼 review 判断。
type eagerRingBuffer[T any] struct {
	ring             []T
	headPos, tailPos int
	full             bool
}

func (r *eagerRingBuffer[T]) Init(size int) { r.ring = make([]T, size) }

func (r *eagerRingBuffer[T]) Len() int {
	if r.full {
		return len(r.ring)
	}
	if r.tailPos >= r.headPos {
		return r.tailPos - r.headPos
	}
	return r.tailPos - r.headPos + len(r.ring)
}

func (r *eagerRingBuffer[T]) Empty() bool { return !r.full && r.headPos == r.tailPos }

func (r *eagerRingBuffer[T]) PushBack(t T) {
	if r.full || len(r.ring) == 0 {
		r.grow()
	}
	r.ring[r.tailPos] = t
	r.tailPos++
	if r.tailPos == len(r.ring) {
		r.tailPos = 0
	}
	if r.tailPos == r.headPos {
		r.full = true
	}
}

func (r *eagerRingBuffer[T]) PopFront() T {
	r.full = false
	t := r.ring[r.headPos]
	r.ring[r.headPos] = *new(T)
	r.headPos++
	if r.headPos == len(r.ring) {
		r.headPos = 0
	}
	return t
}

func (r *eagerRingBuffer[T]) Offset(index int) *T {
	offset := (r.headPos + index) % len(r.ring)
	return &r.ring[offset]
}

func (r *eagerRingBuffer[T]) grow() {
	oldRing := r.ring
	newSize := len(oldRing) * 2
	if newSize == 0 {
		newSize = 1
	}
	r.ring = make([]T, newSize)
	headLen := copy(r.ring, oldRing[r.headPos:])
	copy(r.ring[headLen:], oldRing[:r.headPos])
	r.headPos, r.tailPos, r.full = 0, len(oldRing), false
}

// TestInitDoesNotPreallocate 是这个补丁的核心断言：Init 不许再分配。
//
// 这条如果被回退（例如有人 `go mod vendor` 冲掉、或上游 rebase 覆盖），
// 编译照过、行为照对，只有内存悄悄涨回去 —— 所以必须由测试说话。
func TestInitDoesNotPreallocate(t *testing.T) {
	var r RingBuffer[connectionStateOnSentPacket]
	r.Init(defaultConnectionStateMapQueueSize)
	if got := cap(r.ring); got != 0 {
		t.Fatalf("Init 仍在预分配：cap(ring)=%d，期望 0", got)
	}
	if !r.Empty() || r.Len() != 0 {
		t.Fatalf("Init 后应为空：Empty=%v Len=%d", r.Empty(), r.Len())
	}
}

// TestGrowStartsAtInitialRingSize 钉住第一次增长的落点。
func TestGrowStartsAtInitialRingSize(t *testing.T) {
	var r RingBuffer[int]
	r.Init(256)
	r.PushBack(1)
	if got := len(r.ring); got != initialRingSize {
		t.Fatalf("首次 grow 落到 %d，期望 %d", got, initialRingSize)
	}
	if r.Len() != 1 || *r.Front() != 1 {
		t.Fatalf("首次 PushBack 之后状态不对：Len=%d", r.Len())
	}
}

// TestLazyMatchesEagerOnRandomOps 对拍：同一串随机操作，惰性版与原版
// 必须在**每一步**给出相同的 Len/Empty/Front/Back/Offset 全序。
func TestLazyMatchesEagerOnRandomOps(t *testing.T) {
	for _, initSize := range []int{0, 1, 8, 256} {
		rng := rand.New(rand.NewSource(int64(initSize) + 20260803))
		var lazy RingBuffer[int]
		var eager eagerRingBuffer[int]
		lazy.Init(initSize)
		eager.Init(initSize)

		next := 0
		for step := 0; step < 20000; step++ {
			if rng.Intn(100) < 55 || eager.Empty() {
				lazy.PushBack(next)
				eager.PushBack(next)
				next++
			} else {
				gotL, gotE := lazy.PopFront(), eager.PopFront()
				if gotL != gotE {
					t.Fatalf("init=%d step=%d PopFront 不一致：lazy=%d eager=%d", initSize, step, gotL, gotE)
				}
			}
			if lazy.Len() != eager.Len() || lazy.Empty() != eager.Empty() {
				t.Fatalf("init=%d step=%d Len/Empty 不一致：lazy=(%d,%v) eager=(%d,%v)",
					initSize, step, lazy.Len(), lazy.Empty(), eager.Len(), eager.Empty())
			}
			if n := lazy.Len(); n > 0 {
				for _, i := range []int{0, n / 2, n - 1} {
					if *lazy.Offset(i) != *eager.Offset(i) {
						t.Fatalf("init=%d step=%d Offset(%d) 不一致", initSize, step, i)
					}
				}
				if *lazy.Front() != *eager.Offset(0) || *lazy.Back() != *eager.Offset(n-1) {
					t.Fatalf("init=%d step=%d Front/Back 不一致", initSize, step)
				}
			}
		}
	}
}

// TestClearOnLazyBufferIsSafe —— Clear 会遍历 ring，nil ring 必须不 panic。
func TestClearOnLazyBufferIsSafe(t *testing.T) {
	var r RingBuffer[int]
	r.Init(256)
	r.Clear()
	if !r.Empty() || r.Len() != 0 {
		t.Fatalf("Clear 之后应为空")
	}
	r.PushBack(7)
	r.Clear()
	if !r.Empty() {
		t.Fatalf("Clear 之后应为空")
	}
}

// TestIdleConnectionFootprintStaysSmall 复现生产上的空闲连接：
// HY2 客户端每 10s 一个 keepalive，服务端 sampler 每次记一个包、随即被
// ack 掉。这种模式下 packetNumberIndexedQueue 的槽位不该增长。
//
// 这就是省内存的机制本身 —— 它必须被断言，而不是被相信。
func TestIdleConnectionFootprintStaysSmall(t *testing.T) {
	q := newPacketNumberIndexedQueue[connectionStateOnSentPacket](defaultConnectionStateMapQueueSize)
	entry := connectionStateOnSentPacket{size: 1200}
	for pn := congestion.PacketNumber(1); pn <= 5000; pn++ {
		if !q.Emplace(pn, &entry) {
			t.Fatalf("Emplace(%d) 失败", pn)
		}
		q.RemoveUpTo(pn + 1)
	}
	if got := q.EntrySlotsUsed(); got != 0 {
		t.Fatalf("空闲连接残留 %d 个槽位，期望 0", got)
	}
	if got := cap(q.entries.ring); got > initialRingSize {
		t.Fatalf("空闲连接的 ring 长到了 %d 槽，期望 ≤%d（惰性化失效）", got, initialRingSize)
	}
}

// TestBusyConnectionStillGrows —— 反向对照：满速连接必须照样拿到大 buffer，
// 否则「省内存」就是把吞吐偷偷换掉了。
func TestBusyConnectionStillGrows(t *testing.T) {
	q := newPacketNumberIndexedQueue[connectionStateOnSentPacket](defaultConnectionStateMapQueueSize)
	entry := connectionStateOnSentPacket{size: 1200}
	const inFlight = 1024
	for pn := congestion.PacketNumber(1); pn <= inFlight; pn++ {
		if !q.Emplace(pn, &entry) {
			t.Fatalf("Emplace(%d) 失败", pn)
		}
	}
	if got := q.EntrySlotsUsed(); got != inFlight {
		t.Fatalf("满速连接槽位 %d，期望 %d", got, inFlight)
	}
	if got := cap(q.entries.ring); got < inFlight {
		t.Fatalf("满速连接 ring 只有 %d 槽，装不下 %d 个在途包", got, inFlight)
	}
	for pn := congestion.PacketNumber(1); pn <= inFlight; pn++ {
		if q.GetEntry(pn) == nil {
			t.Fatalf("满速连接丢了包 %d 的状态", pn)
		}
	}
}

// TestBandwidthSamplerIdleHeapFootprint 量化收益：1000 条空闲连接的
// sampler，堆占用相对预分配版的下降幅度。生产 pprof 给的是 32.7MB/132MB，
// 这里在单测里复算一遍，免得日后有人只凭直觉调 initialRingSize。
func TestBandwidthSamplerIdleHeapFootprint(t *testing.T) {
	const conns = 1000
	measure := func(build func() any) uint64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		keep := make([]any, 0, conns)
		for i := 0; i < conns; i++ {
			keep = append(keep, build())
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(keep)
		return after.HeapAlloc - before.HeapAlloc
	}

	lazyBytes := measure(func() any { return newBandwidthSampler(0) })
	eagerBytes := measure(func() any {
		type eagerSampler struct {
			states     eagerRingBuffer[entryWrapper[connectionStateOnSentPacket]]
			candidates eagerRingBuffer[ackPoint]
		}
		s := &eagerSampler{}
		s.states.Init(defaultConnectionStateMapQueueSize)
		s.candidates.Init(defaultCandidatesBufferSize)
		return s
	})

	perConnSaved := float64(eagerBytes-lazyBytes) / conns
	t.Logf("%d 条空闲连接：惰性 %d B，预分配基准 %d B，每连接省 ~%.0f B",
		conns, lazyBytes, eagerBytes, perConnSaved)
	if perConnSaved < 20*1024 {
		t.Fatalf("每连接只省了 %.0f B，低于预期的 ~26KB —— 补丁没生效或被削弱", perConnSaved)
	}
}
