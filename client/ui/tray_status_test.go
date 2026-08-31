//go:build !android && !ios && !freebsd && !js

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/netbirdio/netbird/client/ui/services"
)

// The daemon has no disconnecting state: it reports Connected right up until
// the tunnel is down. A slow Down would therefore leave the row claiming a
// connection that is going away — and the switch reading on again as soon as
// the next status push landed, or the menu was reopened.
func TestEffectiveStatusWhileDisconnecting(t *testing.T) {
	tray := &Tray{connected: true, lastStatus: services.StatusConnected}

	status, connected := tray.effectiveStatusLocked()
	assert.Equal(t, services.StatusConnected, status, "the daemon's status stands on its own")
	assert.True(t, connected)

	tray.disconnecting = true

	status, connected = tray.effectiveStatusLocked()
	assert.Equal(t, statusDisconnecting, status, "a Down in flight is what the row must say")
	assert.False(t, connected, "the switch must not read on while the tunnel is going down")
}

// Both sentinels are tray-only, so nothing downstream may confuse one for a
// status the daemon can send.
func TestStatusSentinelsAreDistinct(t *testing.T) {
	daemonStatuses := []string{
		services.StatusConnected,
		services.StatusConnecting,
		services.StatusIdle,
		services.StatusNeedsLogin,
		services.StatusLoginFailed,
		services.StatusSessionExpired,
		services.StatusDaemonUnavailable,
	}

	for _, status := range daemonStatuses {
		assert.NotEqual(t, statusDisconnecting, status)
		assert.NotEqual(t, statusError, status)
	}
	assert.NotEqual(t, statusError, statusDisconnecting)
}
