package server

import (
	"errors"
	"io"
	"sync"
	"time"
)

var errDisconnect = errors.New("traffic logger requested disconnect")

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
	level := 0
	bufp := copyBufPools[level].Get().(*[]byte)
	buf := *bufp
	defer func() { copyBufPools[level].Put(bufp) }()
	fullReads := 0

	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			if !log(uint64(nr)) {
				// Log returns false, which means that the client should be disconnected
				return errDisconnect
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

func copyTwoWayEx(id string, serverRw, remoteRw io.ReadWriter, l TrafficLogger, stats *StreamStats) error {
	errChan := make(chan error, 2)
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
