package bbr

// initialRingSize 是 ring 第一次增长时直接跳到的容量。
//
// 取 8 而不是 1，是为了让「上来就有流量」的连接少走几次翻倍：从 8 到 256 只有
// 5 次 grow，累计复制 248 个元素（约 22KB memmove），一条连接一生只付一次，
// 相对于每条连接省下的 22KB 常驻堆完全不值一提。取 1 则要走 8 次。
const initialRingSize = 8

// A RingBuffer is a ring buffer.
// It acts as a heap that doesn't cause any allocations.
type RingBuffer[T any] struct {
	ring             []T
	headPos, tailPos int
	full             bool
}

// Init 重置 ring buffer，并**故意不预分配** size 个元素。
//
// 为什么（2026-08-03 生产实测，yue-br 落地节点）：
//
//	原实现 `make([]T, size)` 在每条 QUIC 连接建立时就分配满额。bandwidthSampler
//	对每条连接 Init 两个 ring：connectionStateMap 256 × 88B = 22.5KB，
//	a0Candidates 256 × 16B = 4KB。落地节点上 HY2 客户端默认每 10s 发一次
//	keepalive（core/client/config.go:defaultKeepAlivePeriod），所以一台低流量机
//	常驻 ~1310 条 QUIC 连接却只有 6 条真在转发的流。pprof -inuse_space 实测：
//	这两个 Init 占 32.7MB / 132MB 存活堆 = 24.7%，全部是空闲连接的预分配。
//
//	而这些 buffer 的实际高水位由「在途包数」决定：空闲连接每 10s 一个 PING，
//	packetNumberIndexedQueue.clearup() 立刻把 front 弹掉，Len() 长期是个位数。
//	预分配 256 是给满速连接准备的，却按连接数收费。
//
//	改成按需增长后，空闲连接常驻 8 槽（704B + 128B），满速连接照样长到 256+，
//	行为与原实现逐操作等价（见 ringbuffer_lazy_test.go 的随机序列对拍）。
//
// size 保留在签名里只为不动调用方，且它仍诚实地描述「预期上限」。
func (r *RingBuffer[T]) Init(size int) {
	_ = size
	r.ring = nil
	r.headPos, r.tailPos, r.full = 0, 0, false
}

// Len returns the number of elements in the ring buffer.
func (r *RingBuffer[T]) Len() int {
	if r.full {
		return len(r.ring)
	}
	if r.tailPos >= r.headPos {
		return r.tailPos - r.headPos
	}
	return r.tailPos - r.headPos + len(r.ring)
}

// Empty says if the ring buffer is empty.
func (r *RingBuffer[T]) Empty() bool {
	return !r.full && r.headPos == r.tailPos
}

// PushBack adds a new element.
// If the ring buffer is full, its capacity is increased first.
func (r *RingBuffer[T]) PushBack(t T) {
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

// PopFront returns the next element.
// It must not be called when the buffer is empty, that means that
// callers might need to check if there are elements in the buffer first.
func (r *RingBuffer[T]) PopFront() T {
	if r.Empty() {
		panic("github.com/quic-go/quic-go/internal/utils/ringbuffer: pop from an empty queue")
	}
	r.full = false
	t := r.ring[r.headPos]
	r.ring[r.headPos] = *new(T)
	r.headPos++
	if r.headPos == len(r.ring) {
		r.headPos = 0
	}
	return t
}

// Offset returns the offset element.
// It must not be called when the buffer is empty, that means that
// callers might need to check if there are elements in the buffer first
// and check if the index larger than buffer length.
func (r *RingBuffer[T]) Offset(index int) *T {
	if r.Empty() || index >= r.Len() {
		panic("github.com/quic-go/quic-go/internal/utils/ringbuffer: offset from invalid index")
	}
	offset := (r.headPos + index) % len(r.ring)
	return &r.ring[offset]
}

// Front returns the front element.
// It must not be called when the buffer is empty, that means that
// callers might need to check if there are elements in the buffer first.
func (r *RingBuffer[T]) Front() *T {
	if r.Empty() {
		panic("github.com/quic-go/quic-go/internal/utils/ringbuffer: front from an empty queue")
	}
	return &r.ring[r.headPos]
}

// Back returns the back element.
// It must not be called when the buffer is empty, that means that
// callers might need to check if there are elements in the buffer first.
func (r *RingBuffer[T]) Back() *T {
	if r.Empty() {
		panic("github.com/quic-go/quic-go/internal/utils/ringbuffer: back from an empty queue")
	}
	return r.Offset(r.Len() - 1)
}

// Grow the maximum size of the queue.
// This method assume the queue is full.
func (r *RingBuffer[T]) grow() {
	oldRing := r.ring
	newSize := len(oldRing) * 2
	if newSize == 0 {
		newSize = initialRingSize
	}
	r.ring = make([]T, newSize)
	headLen := copy(r.ring, oldRing[r.headPos:])
	copy(r.ring[headLen:], oldRing[:r.headPos])
	r.headPos, r.tailPos, r.full = 0, len(oldRing), false
}

// Clear removes all elements.
func (r *RingBuffer[T]) Clear() {
	var zeroValue T
	for i := range r.ring {
		r.ring[i] = zeroValue
	}
	r.headPos, r.tailPos, r.full = 0, 0, false
}
