package server

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var errDisconnect = errors.New("traffic logger requested disconnect")

// errStreamRejected ends one TCP stream at a TrafficVerdictLogger's
// TrafficReject. Unlike errDisconnect it never closes the QUIC connection.
var errStreamRejected = errors.New("traffic logger rejected stream")

const (
	copyBufMinShift = 12 // 4 KiB
	copyBufLevels   = 4  // 4, 8, 16, 32 KiB
	copyGrowReads   = 2
)

// Most proxy streams are idle or interactive. Start them at 4 KiB and grow
// only after sustained full reads; bulk streams still reach 32 KiB quickly.
var copyBufPools = [copyBufLevels]sync.Pool{
	{New: func() any { b := make([]byte, 1<<copyBufMinShift); return &b }},
	{New: func() any { b := make([]byte, 2<<copyBufMinShift); return &b }},
	{New: func() any { b := make([]byte, 4<<copyBufMinShift); return &b }},
	{New: func() any { b := make([]byte, 8<<copyBufMinShift); return &b }},
}

func copyBufferLog(dst io.Writer, src io.Reader, log func(n uint64) bool) error {
	return copyBufferCheck(dst, src, func(n uint64) error {
		if !log(n) {
			// Log returns false, which means that the client should be disconnected
			return errDisconnect
		}
		return nil
	})
}

// copyBufferCheck is copyBufferLog with a three-way check: a non-nil error
// from check ends the copy with that error before the chunk is written.
func copyBufferCheck(dst io.Writer, src io.Reader, check func(n uint64) error) error {
	level := 0
	bufp := copyBufPools[level].Get().(*[]byte)
	buf := *bufp
	defer func() { copyBufPools[level].Put(bufp) }()
	fullReads := 0

	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			if err := check(uint64(nr)); err != nil {
				return err
			}
			nw, ew := dst.Write(buf[0:nr])
			if ew != nil {
				return ew
			}
			if nw != nr {
				return io.ErrShortWrite
			}

			if nr == len(buf) {
				fullReads++
			} else {
				fullReads = 0
			}
			if fullReads >= copyGrowReads && level+1 < len(copyBufPools) {
				copyBufPools[level].Put(bufp)
				level++
				bufp = copyBufPools[level].Get().(*[]byte)
				buf = *bufp
				fullReads = 0
			}
		}
		if er != nil {
			if er == io.EOF {
				// EOF should not be considered as an error
				return nil
			}
			return er
		}
	}
}

// copyTwoWayEx proxies with per-chunk traffic logging. stats may be nil, in
// which case only LogTraffic runs per chunk (see StreamStatsOptOut).
func copyTwoWayEx(ctx context.Context, id string, serverRw, remoteRw io.ReadWriter, l TrafficLogger, stats *StreamStats) error {
	if vl, ok := l.(TrafficVerdictLogger); ok {
		return copyTwoWayVerdict(ctx, id, serverRw, remoteRw, vl, stats)
	}
	errChan := make(chan error, 2)
	if stats == nil {
		go func() {
			errChan <- copyBufferLog(serverRw, remoteRw, func(n uint64) bool {
				return l.LogTraffic(id, 0, n)
			})
		}()
		go func() {
			errChan <- copyBufferLog(remoteRw, serverRw, func(n uint64) bool {
				return l.LogTraffic(id, n, 0)
			})
		}()
		return <-errChan
	}
	go func() {
		errChan <- copyBufferLog(serverRw, remoteRw, func(n uint64) bool {
			stats.LastActiveTime.Store(time.Now())
			stats.Rx.Add(n)
			return l.LogTraffic(id, 0, n)
		})
	}()
	go func() {
		errChan <- copyBufferLog(remoteRw, serverRw, func(n uint64) bool {
			stats.LastActiveTime.Store(time.Now())
			stats.Tx.Add(n)
			return l.LogTraffic(id, n, 0)
		})
	}()
	// Block until one of the two goroutines returns
	return <-errChan
}

// streamVerdictError maps a TrafficVerdict to the copy loop's result.
func streamVerdictError(v TrafficVerdict) error {
	switch v {
	case TrafficAccept:
		return nil
	case TrafficReject:
		return errStreamRejected
	default:
		return errDisconnect
	}
}

// copyTwoWayVerdict is copyTwoWayEx for a TrafficVerdictLogger (yue fork):
// TrafficReject ends only this stream (errStreamRejected), TrafficDisconnect
// the connection (errDisconnect). stats may be nil as in copyTwoWayEx.
func copyTwoWayVerdict(ctx context.Context, id string, serverRw, remoteRw io.ReadWriter, l TrafficVerdictLogger, stats *StreamStats) error {
	logTraffic := l.LogStreamTraffic
	if contextual, ok := l.(ContextTrafficVerdictLogger); ok {
		// The first completed copy causes its caller to close both transports.
		// Cancel the other direction's limiter wait at that same boundary,
		// including when only one peer closed and the QUIC connection survives.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		logTraffic = func(id string, tx, rx uint64) TrafficVerdict {
			return contextual.LogStreamTrafficContext(ctx, id, tx, rx)
		}
	}
	errChan := make(chan error, 2)
	go func() {
		errChan <- copyBufferCheck(serverRw, remoteRw, func(n uint64) error {
			if stats != nil {
				stats.LastActiveTime.Store(time.Now())
				stats.Rx.Add(n)
			}
			return streamVerdictError(logTraffic(id, 0, n))
		})
	}()
	go func() {
		errChan <- copyBufferCheck(remoteRw, serverRw, func(n uint64) error {
			if stats != nil {
				stats.LastActiveTime.Store(time.Now())
				stats.Tx.Add(n)
			}
			return streamVerdictError(logTraffic(id, n, 0))
		})
	}()
	// Block until one of the two goroutines returns
	return <-errChan
}

// copyTwoWay is the "fast-path" version of copyTwoWayEx that does not log traffic or update stream stats.
// It uses the built-in io.Copy instead of our own copyBufferLog.
func copyTwoWay(serverRw, remoteRw io.ReadWriter) error {
	errChan := make(chan error, 2)
	go func() {
		_, err := io.Copy(serverRw, remoteRw)
		errChan <- err
	}()
	go func() {
		_, err := io.Copy(remoteRw, serverRw)
		errChan <- err
	}()
	// Block until one of the two goroutines returns
	return <-errChan
}
