//go:build !android && !ios && !freebsd && !js

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/netbirdio/netbird/client/ui/services"
)

// The daemon reports Connected until the tunnel is down, so without a state of
// its own the row claims a connection that is going away and the switch reads
// on again on the next status push.
func TestEffectiveStatusWhileDisconnecting(t *testing.T) {
	tray := &Tray{connected: true, lastStatus: services.StatusConnected}

	status, connected := tray.effectiveStatusLocked()
	assert.Equal(t, services.StatusConnected, status)
	assert.True(t, connected)

	tray.disconnecting = true

	status, connected = tray.effectiveStatusLocked()
	assert.Equal(t, statusDisconnecting, status)
	assert.False(t, connected)
}
