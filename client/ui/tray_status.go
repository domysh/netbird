//go:build !android && !ios && !freebsd && !js

package main

import (
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/netbirdio/netbird/client/ui/services"
)

func (t *Tray) onStatusEvent(ev *application.CustomEvent) {
	st, ok := ev.Data.(services.Status)
	if !ok {
		return
	}
	t.applyStatus(st)
}

// applyStatus repaints the tray from a daemon snapshot. Icon refresh is skipped
// when no icon-relevant input changed: the daemon emits rapid SubscribeStatus
// bursts during health probes that would otherwise spam Shell_NotifyIcon.
func (t *Tray) applyStatus(st services.Status) {
	t.statusMu.Lock()
	connected := strings.EqualFold(st.Status, services.StatusConnected)
	iconChanged := connected != t.connected || st.Status != t.lastStatus
	// The daemon re-emits SessionExpired on every snapshot while expired; act
	// only on the transition into it so the notification fires once.
	sessionExpiredEnter := strings.EqualFold(st.Status, services.StatusSessionExpired) &&
		!strings.EqualFold(t.lastStatus, services.StatusSessionExpired)

	triggerLogin := t.consumePendingConnectLogin(st.Status)

	daemonVersionChanged := st.DaemonVersion != "" && st.DaemonVersion != t.lastDaemonVersion
	t.connected = connected
	t.lastStatus = st.Status
	if daemonVersionChanged {
		t.lastDaemonVersion = st.DaemonVersion
	}

	revisionChanged := st.NetworksRevision != t.lastNetworksRevision
	t.lastNetworksRevision = st.NetworksRevision
	t.statusMu.Unlock()

	if triggerLogin {
		t.app.Event.Emit(services.EventTriggerLogin)
	}

	// Cache-only; the row is painted by the relayout below.
	sessionChanged := t.applySessionExpiry(st.SessionExpiresAt, connected)

	if iconChanged {
		t.applyIcon()
	}
	// All repainting goes through refreshMenuState (menuMu-serialised):
	// applyStatus runs concurrently with itself and with relayouts, so
	// unguarded item mutation would race the buildMenu pointer swap. No rows
	// change here, so on macOS this repaints the items in place and a menu the
	// user has open follows the connection live.
	if iconChanged || daemonVersionChanged || sessionChanged {
		t.refreshMenuState()
	}
	// The revision is the only reliable signal: candidate routes never appear
	// in the peer-status snapshot, so a removed exit node would go unnoticed.
	if iconChanged || revisionChanged {
		go t.refreshExitNodes()
	}
	// The daemon emits no active-profile event, so profile flips driven
	// elsewhere (CLI, autoconnect) surface via status transitions.
	if iconChanged {
		go t.loadProfiles()
	}
	if sessionExpiredEnter {
		t.handleSessionExpired()
	}
}

// consumePendingConnectLogin acts on the SSO auto-handoff flag armed by
// handleConnect. Returns true on NeedsLogin so the browser-login flow starts
// without a second Connect click; clears the flag on any terminal state so a
// stale flag can't fire on a later daemon flip. Must hold statusMu.
func (t *Tray) consumePendingConnectLogin(status string) bool {
	if !t.pendingConnectLogin {
		return false
	}
	switch {
	case strings.EqualFold(status, services.StatusNeedsLogin):
		t.pendingConnectLogin = false
		return true
	case strings.EqualFold(status, services.StatusConnected),
		strings.EqualFold(status, services.StatusIdle),
		strings.EqualFold(status, services.StatusLoginFailed),
		strings.EqualFold(status, services.StatusSessionExpired),
		strings.EqualFold(status, services.StatusDaemonUnavailable):
		t.pendingConnectLogin = false
	}
	return false
}

// applyStatusIndicator sets the status dot bitmap. Call only from the Wails
// painter (menuMu held) — the menu is closed there, so the hop onto
// the UI thread below is one AppKit actually runs.
//
// live distinguishes the two callers. During a relayout the tree is half-built
// and the item has no platform impl yet, so the bitmap is recorded and rides
// the relayout's trailing SetMenu. On the repaint path the item is live and the
// setter reaches AppKit directly — and setMenuItemBitmap is the one darwin
// setter Wails does not marshal onto the UI thread itself, so hop there first.
func (t *Tray) applyStatusIndicator(item *application.MenuItem, bitmap []byte, live bool) {
	if item == nil {
		return
	}
	if !live {
		item.SetBitmap(bitmap)
		return
	}
	application.InvokeSync(func() { item.SetBitmap(bitmap) })
}

func statusIndicatorBitmap(status string) []byte {
	switch {
	case strings.EqualFold(status, services.StatusConnected):
		return iconMenuDotConnected
	case strings.EqualFold(status, services.StatusConnecting):
		return iconMenuDotConnecting
	case strings.EqualFold(status, services.StatusNeedsLogin),
		strings.EqualFold(status, services.StatusSessionExpired):
		return iconMenuDotConnecting
	case strings.EqualFold(status, services.StatusLoginFailed),
		strings.EqualFold(status, statusError):
		return iconMenuDotError
	case strings.EqualFold(status, services.StatusDaemonUnavailable):
		return iconMenuDotOffline
	default:
		return iconMenuDotIdle
	}
}
