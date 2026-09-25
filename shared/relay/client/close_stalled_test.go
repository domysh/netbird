package client

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/netbirdio/netbird/shared/relay/messages"
)

// stalledConn is a relay connection whose writes block until it is closed,
// like a TCP connection to a relay that stopped acknowledging: the write only
// fails once the kernel gives up retransmitting, minutes later. Like the
// WebSocket and QUIC connections, it has no write deadline.
type stalledConn struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func newStalledConn() *stalledConn {
	return &stalledConn{closed: make(chan struct{})}
}

func (c *stalledConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}

func (c *stalledConn) Write([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}

func (c *stalledConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *stalledConn) LocalAddr() net.Addr  { return &net.TCPAddr{} }
func (c *stalledConn) RemoteAddr() net.Addr { return &net.TCPAddr{} }

func (c *stalledConn) SetDeadline(time.Time) error {
	return errors.New("SetDeadline is not implemented")
}

func (c *stalledConn) SetReadDeadline(time.Time) error {
	return errors.New("SetReadDeadline is not implemented")
}

func (c *stalledConn) SetWriteDeadline(time.Time) error {
	return errors.New("SetWriteDeadline is not implemented")
}

func newStalledClient(t *testing.T) (*Client, *stalledConn) {
	t.Helper()
	conn := newStalledConn()
	t.Cleanup(func() { _ = conn.Close() })

	c := NewClient("rel://relay.invalid:443", nil, "local-peer", 1280)
	c.relayConn = conn
	c.serviceIsRunning = true
	c.stateSubscription = NewPeersStateSubscription(c.log, conn, nil)
	return c, conn
}

// waitReturn fails the test when fn does not return well past the write bound.
func waitReturn(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()

	select {
	case err := <-done:
		return err
	case <-time.After(closeWriteTimeout + 5*time.Second):
		t.Fatalf("%s blocked on a stalled relay write", what)
		return nil
	}
}

func TestCloseWithStalledRelayWrite(t *testing.T) {
	c, conn := newStalledClient(t)

	err := waitReturn(t, "Close", c.Close)

	assert.NoError(t, err)
	assert.False(t, c.Ready(), "the client should report it stopped")
	select {
	case <-conn.closed:
	default:
		t.Error("the relay connection should be closed, releasing the pending close message write")
	}
}

func TestCloseConnWithStalledRelayWrite(t *testing.T) {
	c, _ := newStalledClient(t)

	peerID := messages.HashID("remote-peer")
	container := newConnContainer(c.log, c, peerID, nil)
	c.mu.Lock()
	c.conns[peerID] = container
	c.mu.Unlock()

	// Closing the relayed peer connection unsubscribes from the peer's state
	// while holding the client lock, which every other relay operation needs.
	err := waitReturn(t, "closing a relayed peer connection", container.conn.Close)

	assert.NoError(t, err)
	c.mu.Lock()
	_, stillOpen := c.conns[peerID]
	c.mu.Unlock()
	assert.False(t, stillOpen, "the peer connection should be released even though the unsubscribe stalled")

	require.NoError(t, waitReturn(t, "Close", c.Close))
}
