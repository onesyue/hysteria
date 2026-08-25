package server

import (
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"errors"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	"github.com/apernet/quic-go/quicvarint"

	"github.com/apernet/hysteria/core/v2/internal/congestion"
	"github.com/apernet/hysteria/core/v2/internal/protocol"
	"github.com/apernet/hysteria/core/v2/internal/utils"
)

const (
	closeErrCodeOK                  = 0x100 // HTTP3 ErrCodeNoError
	closeErrCodeTrafficLimitReached = 0x107 // HTTP3 ErrCodeExcessiveLoad
)

type Server interface {
	Serve() error
	Close() error
}

func convertToStdTLSConfig(config *Config) *tls.Config {
	var clientAuth tls.ClientAuthType
	if config.TLSConfig.ClientCAs != nil {
		clientAuth = tls.RequireAndVerifyClientCert
	} else {
		clientAuth = tls.NoClientCert
	}
	return http3.ConfigureTLSConfig(&tls.Config{
		Certificates:                config.TLSConfig.Certificates,
		GetCertificate:              config.TLSConfig.GetCertificate,
		ClientCAs:                   config.TLSConfig.ClientCAs,
		ClientAuth:                  clientAuth,
		EncryptedClientHelloKeys:    config.TLSConfig.ECHKeys,
		GetEncryptedClientHelloKeys: config.TLSConfig.GetECHKeys,
	})
}

func NewServer(config *Config) (Server, error) {
	if err := config.fill(); err != nil {
		return nil, err
	}
	tlsConfig := convertToStdTLSConfig(config)
	quicConfig := &quic.Config{
		InitialStreamReceiveWindow:     config.QUICConfig.InitialStreamReceiveWindow,
		MaxStreamReceiveWindow:         config.QUICConfig.MaxStreamReceiveWindow,
		InitialConnectionReceiveWindow: config.QUICConfig.InitialConnectionReceiveWindow,
		MaxConnectionReceiveWindow:     config.QUICConfig.MaxConnectionReceiveWindow,
		AllowConnectionWindowIncrease:  config.QUICConfig.AllowConnectionWindowIncrease,
		AllowConnectionReceive:         config.QUICConfig.AllowConnectionReceive,
		ReleaseConnectionReceive:       config.QUICConfig.ReleaseConnectionReceive,
		NotifyConnectionClosed:         config.QUICConfig.NotifyConnectionClosed,
		MaxIdleTimeout:                 config.QUICConfig.MaxIdleTimeout,
		MaxIncomingStreams:             config.QUICConfig.MaxIncomingStreams,
		DisablePathMTUDiscovery:        config.QUICConfig.DisablePathMTUDiscovery,
		EnableDatagrams:                true,
		MaxDatagramFrameSize:           protocol.MaxDatagramFrameSize,
		AssumePeerMaxDatagramFrameSize: protocol.MaxDatagramFrameSize,
		DisablePathManager:             true,
	}
	srk := config.StatelessResetKey
	if srk == nil {
		var k quic.StatelessResetKey
		if _, err := crand.Read(k[:]); err != nil {
			return nil, err
		}
		srk = &k
	}
	tr := &quic.Transport{Conn: config.Conn, StatelessResetKey: srk, DisableGSO: config.QUICConfig.DisableGSO}
	listener, err := tr.Listen(tlsConfig, quicConfig)
	if err != nil {
		err = errors.Join(err, tr.Close(), config.Conn.Close())
		if config.Cleanup != nil {
			err = errors.Join(err, config.Cleanup.Close())
		}
		return nil, err
	}
	return &serverImpl{
		config:    config,
		tr:        tr,
		listener:  listener,
		conns:     make(map[*quic.Conn]struct{}),
		closeDone: make(chan struct{}),
	}, nil
}

type serverImpl struct {
	config   *Config
	tr       *quic.Transport
	listener *quic.Listener

	mutex     sync.Mutex
	closing   bool
	conns     map[*quic.Conn]struct{}
	serveWG   sync.WaitGroup
	handlerWG sync.WaitGroup

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func (s *serverImpl) Serve() error {
	s.mutex.Lock()
	if s.closing {
		s.mutex.Unlock()
		return quic.ErrServerClosed
	}
	s.serveWG.Add(1)
	s.mutex.Unlock()
	defer s.serveWG.Done()

	for {
		conn, err := s.listener.Accept(context.Background())
		if err != nil {
			return err
		}

		// Register accepted connections before publishing the handler goroutine.
		// Close sets closing under the same lock and waits for Serve to return,
		// so no connection can slip past the shutdown snapshot.
		s.mutex.Lock()
		if s.closing {
			s.mutex.Unlock()
			_ = conn.CloseWithError(closeErrCodeOK, "")
			continue
		}
		s.conns[conn] = struct{}{}
		s.handlerWG.Add(1)
		s.mutex.Unlock()
		go s.serveClient(conn)
	}
}

func (s *serverImpl) Close() error {
	s.closeOnce.Do(func() {
		defer close(s.closeDone)

		s.mutex.Lock()
		s.closing = true
		s.mutex.Unlock()

		// Stop Accept first, then wait until every accepted connection has either
		// been registered or closed by Serve. Listener.Close deliberately leaves
		// established QUIC connections alive.
		s.closeErr = s.listener.Close()
		s.serveWG.Wait()

		s.mutex.Lock()
		conns := make([]*quic.Conn, 0, len(s.conns))
		for conn := range s.conns {
			conns = append(conns, conn)
		}
		s.mutex.Unlock()

		// Transport.Close is intentionally abrupt and doesn't send CONNECTION_CLOSE.
		// Close established connections first so peers learn about shutdown now,
		// instead of waiting for their idle timeout. Do it concurrently so shutdown
		// latency doesn't grow linearly with the number of clients.
		connErrs := make([]error, len(conns))
		var closeWG sync.WaitGroup
		for i, conn := range conns {
			closeWG.Add(1)
			go func() {
				defer closeWG.Done()
				connErrs[i] = conn.CloseWithError(closeErrCodeOK, "")
			}()
		}
		closeWG.Wait()
		s.closeErr = errors.Join(s.closeErr, errors.Join(connErrs...))

		// A returned Close is a lifecycle barrier: HTTP/3 handlers have observed
		// connection closure before the UDP transport and cleanup resources go away.
		s.handlerWG.Wait()
		s.closeErr = errors.Join(s.closeErr, s.tr.Close(), s.config.Conn.Close())
		if s.config.Cleanup != nil {
			s.closeErr = errors.Join(s.closeErr, s.config.Cleanup.Close())
		}
	})
	<-s.closeDone
	return s.closeErr
}

func (s *serverImpl) serveClient(conn *quic.Conn) {
	defer s.handlerWG.Done()
	defer func() {
		s.mutex.Lock()
		delete(s.conns, conn)
		s.mutex.Unlock()
	}()
	s.handleClient(conn)
}

func (s *serverImpl) handleClient(conn *quic.Conn) {
	handler := newH3sHandler(s.config, conn)
	h3s := http3.Server{
		Handler:          handler,
		StreamDispatcher: handler.ProxyStreamHijacker,
	}
	err := h3s.ServeQUICConn(conn)
	// StreamDispatcher transfers ownership of proxy streams to h3sHandler, so
	// http3.Server's own wait group can't account for them. Join those workers
	// before publishing this connection as fully closed.
	handler.backgroundWG.Wait()
	authenticated, authID, untrack := handler.finish()
	if untrack != nil {
		untrack()
	}
	// If the client is authenticated, we need to log the disconnect event
	if authenticated {
		if tl := s.config.TrafficLogger; tl != nil {
			tl.LogOnlineState(authID, false)
		}
		if el := s.config.EventLogger; el != nil {
			el.Disconnect(conn.RemoteAddr(), authID, err)
		}
	}
	_ = conn.CloseWithError(closeErrCodeOK, "")
}

type h3sHandler struct {
	config *Config
	conn   *quic.Conn

	authenticated bool
	authMutex     sync.Mutex
	authID        string
	untrack       func()
	connID        uint32 // a random id for dump streams

	backgroundWG sync.WaitGroup
}

func (h *h3sHandler) finish() (authenticated bool, authID string, untrack func()) {
	h.authMutex.Lock()
	defer h.authMutex.Unlock()
	untrack = h.untrack
	h.untrack = nil
	return h.authenticated, h.authID, untrack
}

func newH3sHandler(config *Config, conn *quic.Conn) *h3sHandler {
	return &h3sHandler{
		config: config,
		conn:   conn,
		connID: rand.Uint32(),
	}
}

func (h *h3sHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.Host == protocol.URLHost && r.URL.Path == protocol.URLPath {
		h.authMutex.Lock()
		defer h.authMutex.Unlock()
		if h.authenticated {
			// Already authenticated
			protocol.AuthResponseToHeader(w.Header(), protocol.AuthResponse{
				UDPEnabled: !h.config.DisableUDP,
				Rx:         h.config.BandwidthConfig.MaxRx,
				RxAuto:     h.config.IgnoreClientBandwidth,
			})
			w.WriteHeader(protocol.StatusAuthOK)
			return
		}
		authReq := protocol.AuthRequestFromHeader(r.Header)
		actualTx := authReq.Rx
		ok, id := h.config.Authenticator.Authenticate(h.conn.RemoteAddr(), authReq.Auth, actualTx)
		if ok {
			if tracker, ok := h.config.TrafficLogger.(ConnectionTracker); ok {
				var accepted bool
				h.untrack, accepted = tracker.TrackConnection(id, func() error {
					return h.conn.CloseWithError(closeErrCodeTrafficLimitReached, "")
				})
				if !accepted {
					// Authorization changed between Authenticate and registration.
					// Never publish a successful response for that stale result.
					h.masqHandler(w, r)
					return
				}
			}
			// Publish the authenticated state only after registration succeeds.
			h.authenticated = true
			h.authID = id
			if h.config.IgnoreClientBandwidth {
				// Ignore client bandwidth and use the configured congestion controller.
				congestion.UseConfigured(h.conn, h.config.CongestionConfig.Type, h.config.CongestionConfig.BBRProfile)
				actualTx = 0
			} else {
				// actualTx = min(serverTx, clientRx)
				if h.config.BandwidthConfig.MaxTx > 0 && actualTx > h.config.BandwidthConfig.MaxTx {
					// We have a maxTx limit and the client is asking for more than that,
					// return and use the limit instead
					actualTx = h.config.BandwidthConfig.MaxTx
				}
				if actualTx > 0 {
					congestion.UseBrutal(h.conn, actualTx, h.config.BandwidthConfig.DisableLossCompensation)
				} else {
					// Client doesn't know its own bandwidth, use the configured congestion controller.
					congestion.UseConfigured(h.conn, h.config.CongestionConfig.Type, h.config.CongestionConfig.BBRProfile)
				}
			}
			// Auth OK, send response
			protocol.AuthResponseToHeader(w.Header(), protocol.AuthResponse{
				UDPEnabled: !h.config.DisableUDP,
				Rx:         h.config.BandwidthConfig.MaxRx,
				RxAuto:     h.config.IgnoreClientBandwidth,
			})
			w.WriteHeader(protocol.StatusAuthOK)
			// Call event logger
			if tl := h.config.TrafficLogger; tl != nil {
				tl.LogOnlineState(id, true)
			}
			if el := h.config.EventLogger; el != nil {
				el.Connect(h.conn.RemoteAddr(), id, actualTx)
			}
			// Initialize UDP session manager (if UDP is enabled)
			// We use sync.Once to make sure that only one goroutine is started,
			// as ServeHTTP may be called by multiple goroutines simultaneously
			if !h.config.DisableUDP {
				h.backgroundWG.Add(1)
				go func() {
					defer h.backgroundWG.Done()
					sm := newUDPSessionManager(
						&udpIOImpl{h.conn, id, h.config.TrafficLogger, h.config.RequestHook, h.config.Outbound},
						&udpEventLoggerImpl{h.conn, id, h.config.EventLogger},
						h.config.UDPIdleTimeout,
					)
					_ = sm.Run()
				}()
			}
		} else {
			// Auth failed, pretend to be a normal HTTP server
			h.masqHandler(w, r)
		}
	} else {
		// Not an auth request, pretend to be a normal HTTP server
		h.masqHandler(w, r)
	}
}

func (h *h3sHandler) ProxyStreamHijacker(ft http3.FrameType, stream *quic.Stream, err error) (bool, error) {
	h.authMutex.Lock()
	authenticated, authID := h.authenticated, h.authID
	h.authMutex.Unlock()
	if err != nil || !authenticated {
		return false, nil
	}

	switch ft {
	case protocol.FrameTypeTCPRequest:
		// StreamDispatcher only peeks the frame type. Consume it so ReadTCPRequest
		// starts at address length, matching pre-upgrade StreamHijacker behavior.
		if _, err := quicvarint.Read(quicvarint.NewReader(stream)); err != nil {
			return false, err
		}
		// Wraps the stream with QStream, which handles Close() properly
		qStream := &utils.QStream{Stream: stream}
		h.backgroundWG.Add(1)
		go func() {
			defer h.backgroundWG.Done()
			h.handleTCPRequest(qStream, authID)
		}()
		return true, nil
	default:
		return false, nil
	}
}

func (h *h3sHandler) handleTCPRequest(stream *utils.QStream, authID string) {
	trafficLogger := h.config.TrafficLogger
	streamStats := &StreamStats{
		AuthID:      authID,
		ConnID:      h.connID,
		InitialTime: time.Now(),
	}
	streamStats.State.Store(StreamStateInitial)
	streamStats.LastActiveTime.Store(time.Now())
	if trafficLogger != nil {
		trafficLogger.TraceStream(stream, streamStats)
		defer trafficLogger.UntraceStream(stream)
	}
	// Set the final state before UntraceStream releases the logger's reference.
	defer func() {
		streamStats.State.Store(StreamStateClosed)
	}()

	// Read request
	reqAddr, err := protocol.ReadTCPRequest(stream)
	if err != nil {
		_ = stream.Close()
		return
	}
	streamStats.ReqAddr.Store(reqAddr)
	// Call the hook if set
	var putback []byte
	var hooked bool
	if h.config.RequestHook != nil {
		hooked = h.config.RequestHook.Check(false, reqAddr)
		// When the hook is enabled, the server should always accept a connection
		// so that the client will send whatever request the hook wants to see.
		// This is essentially a server-side fast-open.
		if hooked {
			streamStats.State.Store(StreamStateHooking)
			_ = protocol.WriteTCPResponse(stream, true, "RequestHook enabled")
			putback, err = h.config.RequestHook.TCP(stream, &reqAddr)
			if err != nil {
				_ = stream.Close()
				return
			}
			streamStats.setHookedReqAddr(reqAddr)
		}
	}
	// Log the event
	if h.config.EventLogger != nil {
		h.config.EventLogger.TCPRequest(h.conn.RemoteAddr(), authID, reqAddr)
	}
	// Dial target
	streamStats.State.Store(StreamStateConnecting)
	tConn, err := h.config.Outbound.TCP(reqAddr)
	if err != nil {
		if !hooked {
			_ = protocol.WriteTCPResponse(stream, false, err.Error())
		}
		_ = stream.Close()
		// Log the error
		if h.config.EventLogger != nil {
			h.config.EventLogger.TCPError(h.conn.RemoteAddr(), authID, reqAddr, err)
		}
		return
	}
	if !hooked {
		_ = protocol.WriteTCPResponse(stream, true, "Connected")
	}
	streamStats.State.Store(StreamStateEstablished)
	// Put back the data if the hook requested
	if len(putback) > 0 {
		n, _ := tConn.Write(putback)
		streamStats.Tx.Add(uint64(n))
	}
	// Start proxying
	if trafficLogger != nil {
		err = copyTwoWayEx(authID, stream, tConn, trafficLogger, streamStats)
	} else {
		// Use the fast path if no traffic logger is set
		err = copyTwoWay(stream, tConn)
	}
	if h.config.EventLogger != nil {
		h.config.EventLogger.TCPError(h.conn.RemoteAddr(), authID, reqAddr, err)
	}
	// Cleanup
	_ = tConn.Close()
	_ = stream.Close()
	// Disconnect the client if TrafficLogger requested
	if err == errDisconnect {
		_ = h.conn.CloseWithError(closeErrCodeTrafficLimitReached, "")
	}
}

func (h *h3sHandler) masqHandler(w http.ResponseWriter, r *http.Request) {
	if h.config.MasqHandler != nil {
		h.config.MasqHandler.ServeHTTP(w, r)
	} else {
		// Return 404 for everything
		http.NotFound(w, r)
	}
}

// udpQUICConn is the slice of *quic.Conn the downstream UDP path actually uses.
//
// It exists so the accounting in SendMessage can be exercised by a test. That
// is not academic: charging before the send double-billed every fragmented
// downstream datagram for as long as native HY2 has run, and no test could
// observe it while this field was a concrete *quic.Conn. *quic.Conn satisfies
// this interface unchanged.
type udpQUICConn interface {
	SendDatagram(b []byte) error
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	CloseWithError(code quic.ApplicationErrorCode, desc string) error
}

// udpIOImpl is the IO implementation for udpSessionManager with TrafficLogger support
type udpIOImpl struct {
	Conn          udpQUICConn
	AuthID        string
	TrafficLogger TrafficLogger
	RequestHook   RequestHook
	Outbound      Outbound
}

func (io *udpIOImpl) ReceiveMessage() (*protocol.UDPMessage, error) {
	for {
		msg, err := io.Conn.ReceiveDatagram(context.Background())
		if err != nil {
			// Connection error, this will stop the session manager
			return nil, err
		}
		udpMsg, err := protocol.ParseUDPMessage(msg)
		if err != nil {
			// Invalid message, this is fine - just wait for the next
			continue
		}
		if io.TrafficLogger != nil {
			ok := io.TrafficLogger.LogTraffic(io.AuthID, uint64(len(udpMsg.Data)), 0)
			if !ok {
				// TrafficLogger requested to disconnect the client
				_ = io.Conn.CloseWithError(closeErrCodeTrafficLimitReached, "")
				return nil, errDisconnect
			}
		}
		return udpMsg, nil
	}
}

func (io *udpIOImpl) SendMessage(buf []byte, msg *protocol.UDPMessage) error {
	// 🚨 Account AFTER the datagram is actually on the wire, never before.
	//
	// sendMessageAutoFrag sends the whole message first and, on
	// quic.DatagramTooLargeError, re-sends it as fragments through this same
	// method. Charging up-front therefore billed the payload twice for every
	// downstream UDP packet above the datagram limit (MaxDatagramFrameSize
	// 1200, so anything past ~1150 bytes): once for the attempt that could
	// never succeed, then once more across the fragments. Measured
	// payload=1400 -> billed 2800, exactly 2.00x. Video, games, DNS and
	// QUIC-over-UDP all live above that threshold.
	//
	// The two early returns below are also non-sends and must not be charged:
	// a serialize overflow is a silent drop, and a failed SendDatagram never
	// left the host.
	//
	// Moving the call after the send means a user can overshoot their limit by
	// at most one datagram before the disconnect lands. That is the correct
	// trade: the alternative — the one being replaced — overstates real usage
	// by 100% on an entire traffic class, and it overstates it permanently.
	msgN := msg.Serialize(buf)
	if msgN < 0 {
		// Message larger than buffer, silent drop
		return nil
	}
	if err := io.Conn.SendDatagram(buf[:msgN]); err != nil {
		return err
	}
	if io.TrafficLogger != nil {
		ok := io.TrafficLogger.LogTraffic(io.AuthID, 0, uint64(len(msg.Data)))
		if !ok {
			// TrafficLogger requested to disconnect the client
			_ = io.Conn.CloseWithError(closeErrCodeTrafficLimitReached, "")
			return errDisconnect
		}
	}
	return nil
}

func (io *udpIOImpl) Hook(data []byte, reqAddr *string) error {
	if io.RequestHook != nil && io.RequestHook.Check(true, *reqAddr) {
		return io.RequestHook.UDP(data, reqAddr)
	} else {
		return nil
	}
}

func (io *udpIOImpl) UDP(reqAddr string) (UDPConn, error) {
	return io.Outbound.UDP(reqAddr)
}

func (io *udpIOImpl) CheckUDP(reqAddr string) error {
	return io.Outbound.CheckUDP(reqAddr)
}

type udpEventLoggerImpl struct {
	Conn        *quic.Conn
	AuthID      string
	EventLogger EventLogger
}

func (l *udpEventLoggerImpl) New(sessionID uint32, reqAddr string) {
	if l.EventLogger != nil {
		l.EventLogger.UDPRequest(l.Conn.RemoteAddr(), l.AuthID, sessionID, reqAddr)
	}
}

func (l *udpEventLoggerImpl) Close(sessionID uint32, err error) {
	if l.EventLogger != nil {
		l.EventLogger.UDPError(l.Conn.RemoteAddr(), l.AuthID, sessionID, err)
	}
}
