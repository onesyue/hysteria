package proxymux

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testLocalAddress = "127.0.0.1:0"

func TestListenSOCKS(t *testing.T) {
	address := testLocalAddress

	sl, err := ListenSOCKS(address)
	require.NoError(t, err)
	closeListenerOnCleanup(t, sl)

	hl, err := ListenHTTP(address)
	require.NoError(t, err)
	closeListenerOnCleanup(t, hl)

	_, err = ListenSOCKS(address)
	require.ErrorIs(t, err, ErrProtocolInUse)
	require.NoError(t, sl.Close())

	sl, err = ListenSOCKS(address)
	require.NoError(t, err)
	closeListenerOnCleanup(t, sl)

	require.NoError(t, sl.Close())
	require.NoError(t, hl.Close())
	requireMuxReleased(t, address)
}

func TestListenHTTP(t *testing.T) {
	address := testLocalAddress

	hl, err := ListenHTTP(address)
	require.NoError(t, err)
	closeListenerOnCleanup(t, hl)

	sl, err := ListenSOCKS(address)
	require.NoError(t, err)
	closeListenerOnCleanup(t, sl)

	_, err = ListenHTTP(address)
	require.ErrorIs(t, err, ErrProtocolInUse)
	require.NoError(t, hl.Close())

	hl, err = ListenHTTP(address)
	require.NoError(t, err)
	closeListenerOnCleanup(t, hl)

	require.NoError(t, hl.Close())
	require.NoError(t, sl.Close())
	requireMuxReleased(t, address)
}

func TestRelease(t *testing.T) {
	address := testLocalAddress

	hl, err := ListenHTTP(address)
	require.NoError(t, err)
	closeListenerOnCleanup(t, hl)
	sl, err := ListenSOCKS(address)
	require.NoError(t, err)
	closeListenerOnCleanup(t, sl)

	require.True(t, globalMuxManager.testAddressExists(address))
	boundAddress := hl.Addr().String()
	probe, err := net.Listen("tcp", boundAddress)
	if err == nil {
		require.NoError(t, probe.Close())
	}
	require.Error(t, err)

	require.NoError(t, hl.Close())
	require.NoError(t, sl.Close())

	requireMuxReleased(t, address)
	lis, err := net.Listen("tcp", boundAddress)
	require.NoError(t, err)
	closeListenerOnCleanup(t, lis)
}

func closeListenerOnCleanup(t *testing.T, listener net.Listener) {
	t.Helper()
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
}

func requireMuxReleased(t *testing.T, address string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return !globalMuxManager.testAddressExists(address)
	}, 5*time.Second, 5*time.Millisecond)
}

func (m *muxManager) testAddressExists(address string) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	_, ok := m.listeners[address]
	return ok
}
