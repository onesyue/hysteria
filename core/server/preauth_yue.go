package server

import (
	"sync"
	"sync/atomic"

	"github.com/apernet/quic-go"
)

// preAuthReceive enforces QUICConfig.PreAuthReceiveLimit (yue fork): the
// received-but-unread bytes one connection may hold before it authenticates.
//
// It wraps the embedder's receive-memory callbacks instead of replacing them,
// so the process budget keeps its own accounting unchanged; the pre-auth cap
// is checked first and is purely per connection.
type preAuthReceive struct {
	limit uint64
	conns sync.Map // *quic.Conn -> *preAuthConn
}

type preAuthConn struct {
	authenticated atomic.Bool
	unread        atomic.Uint64
}

func newPreAuthReceive(limit uint64) *preAuthReceive {
	if limit == 0 {
		return nil
	}
	return &preAuthReceive{limit: limit}
}

func (p *preAuthReceive) state(conn *quic.Conn) *preAuthConn {
	if v, ok := p.conns.Load(conn); ok {
		return v.(*preAuthConn)
	}
	v, _ := p.conns.LoadOrStore(conn, new(preAuthConn))
	return v.(*preAuthConn)
}

// allow charges delta against the connection's pre-auth cap. It returns false
// (and charges nothing) when the cap would be exceeded.
func (p *preAuthReceive) allow(conn *quic.Conn, delta uint64) bool {
	if p == nil || conn == nil || delta == 0 {
		return true
	}
	st := p.state(conn)
	if st.authenticated.Load() {
		return true
	}
	for {
		used := st.unread.Load()
		if used >= p.limit || delta > p.limit-used {
			return false
		}
		if st.unread.CompareAndSwap(used, used+delta) {
			return true
		}
	}
}

// release returns a charge made by allow that the embedder's own callback
// then refused, and bytes the application consumed.
func (p *preAuthReceive) release(conn *quic.Conn, delta uint64) {
	if p == nil || conn == nil || delta == 0 {
		return
	}
	v, ok := p.conns.Load(conn)
	if !ok {
		return
	}
	st := v.(*preAuthConn)
	for {
		used := st.unread.Load()
		next := uint64(0)
		if delta < used {
			next = used - delta
		}
		if st.unread.CompareAndSwap(used, next) {
			return
		}
	}
}

func (p *preAuthReceive) markAuthenticated(conn *quic.Conn) {
	if p == nil || conn == nil {
		return
	}
	p.state(conn).authenticated.Store(true)
}

func (p *preAuthReceive) forget(conn *quic.Conn) {
	if p == nil || conn == nil {
		return
	}
	p.conns.Delete(conn)
}

func (p *preAuthReceive) tracked(conn *quic.Conn) bool {
	if p == nil {
		return false
	}
	_, ok := p.conns.Load(conn)
	return ok
}

// wrapQUICConfig installs the pre-auth cap in front of the embedder's
// receive-memory callbacks. With a nil receiver nothing changes.
func (p *preAuthReceive) wrapQUICConfig(qc *quic.Config) {
	if p == nil {
		return
	}
	nextAllow := qc.AllowConnectionReceive
	nextRelease := qc.ReleaseConnectionReceive
	nextClosed := qc.NotifyConnectionClosed
	qc.AllowConnectionReceive = func(conn *quic.Conn, delta uint64) bool {
		if !p.allow(conn, delta) {
			return false
		}
		if nextAllow != nil && !nextAllow(conn, delta) {
			p.release(conn, delta)
			return false
		}
		return true
	}
	qc.ReleaseConnectionReceive = func(conn *quic.Conn, delta uint64) {
		p.release(conn, delta)
		if nextRelease != nil {
			nextRelease(conn, delta)
		}
	}
	qc.NotifyConnectionClosed = func(conn *quic.Conn) {
		p.forget(conn)
		if nextClosed != nil {
			nextClosed(conn)
		}
	}
}
