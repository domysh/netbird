//go:build !android && !ios && !freebsd && !js

package main

import (
	"context"
	"net/netip"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/netbirdio/netbird/client/ui/services"
)

// exitNodeEntry is one Exit Node submenu row; ID is the network's NetID, the Select/Deselect argument.
type exitNodeEntry struct {
	ID       string
	Selected bool
}

// exitNodeEntries snapshots the cached exit-node rows under exitNodesMu.
func (t *Tray) exitNodeEntries() []exitNodeEntry {
	t.exitNodesMu.Lock()
	defer t.exitNodesMu.Unlock()
	return append([]exitNodeEntry(nil), t.exitNodes...)
}

// exitNodesInMenu returns how many rows the Exit Node submenu currently holds,
// which is what the parent row's enabled state has to follow: the cached
// entries run ahead of it while a rebuild waits for the user to close the menu
// (see relayoutMenu), and enabling the row then would open an empty submenu.
func (t *Tray) exitNodesInMenu() int {
	t.exitNodesMu.Lock()
	defer t.exitNodesMu.Unlock()
	return t.exitNodesShown
}

// exitNodeRowLabel is the submenu row for one node. A "✓ " prefix with a plain
// row, not a checkbox: Wails auto-toggles a checkbox on click before OnClick
// runs, so the deselect/select round-trip would briefly show two checked rows.
func exitNodeRowLabel(n exitNodeEntry) string {
	if n.Selected {
		return "✓ " + n.ID
	}
	return n.ID
}

// fillExitNodeSubmenu paints the rows through Wails. Callers must hold
// exitNodesRebuildMu.
func (t *Tray) fillExitNodeSubmenu(nodes []exitNodeEntry) {
	if t.exitNodeSubmenu == nil {
		return
	}
	t.rememberExitNodeRows(nodes)

	t.exitNodeSubmenu.Clear()
	for _, n := range nodes {
		id := n.ID
		selected := n.Selected
		t.exitNodeSubmenu.Add(exitNodeRowLabel(n)).OnClick(func(*application.Context) {
			t.toggleExitNode(id, selected)
		})
	}
}

// exitNodeLabels renders the rows in the order a native click reports back as
// its index.
func exitNodeLabels(nodes []exitNodeEntry) []string {
	labels := make([]string, 0, len(nodes))
	for _, n := range nodes {
		labels = append(labels, exitNodeRowLabel(n))
	}
	return labels
}

// rememberExitNodeRows records what the submenu is showing: the count gates the
// parent row's enabled state, and the rows themselves resolve a native click
// back to a network id.
func (t *Tray) rememberExitNodeRows(nodes []exitNodeEntry) {
	t.exitNodesMu.Lock()
	t.exitNodesShown = len(nodes)
	t.exitNodesRows = append([]exitNodeEntry(nil), nodes...)
	t.exitNodesMu.Unlock()
}

// handleNativeExitNode resolves the position AppKit reports back to the node it
// was drawn for.
func (t *Tray) handleNativeExitNode(index int) {
	t.exitNodesMu.Lock()
	rows := t.exitNodesRows
	t.exitNodesMu.Unlock()
	if index < 0 || index >= len(rows) {
		return
	}
	t.toggleExitNode(rows[index].ID, rows[index].Selected)
}

// refreshExitNodes sources rows from Networks.List() rather than the Status stream
// because only ListNetworks carries the NetID + selected state Select/Deselect need.
// Serialized by exitNodesRebuildMu against overlapping Status pushes.
func (t *Tray) refreshExitNodes() {
	t.exitNodesRebuildMu.Lock()
	defer t.exitNodesRebuildMu.Unlock()

	t.statusMu.Lock()
	connected := t.connected
	t.statusMu.Unlock()

	var nodes []exitNodeEntry
	if connected {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		list, err := t.svc.Networks.List(ctx)
		cancel()
		if err != nil {
			log.Debugf("tray list networks: %v", err)
			return
		}
		nodes = exitNodesFromNetworks(list)
	}

	log.Infof("tray refreshExitNodes: %d exit node(s)", len(nodes))
	for _, n := range nodes {
		log.Infof("tray exit node: id=%q selected=%v", n.ID, n.Selected)
	}

	t.exitNodesMu.Lock()
	changed := !equalExitNodes(nodes, t.exitNodes)
	t.exitNodes = nodes
	t.exitNodesMu.Unlock()

	if !changed {
		return
	}
	// A menu the user has open takes the new rows through AppKit, and the
	// repaint that follows enables the parent row once they are in. With the
	// menu closed the painter does nothing here and the rows arrive with the
	// relayout, which repaints from the cached entries either way — deferred
	// until the menu closes if one is open.
	t.rememberExitNodeRows(nodes)
	t.painter(true).exitNodes(exitNodeLabels(nodes))
	t.refreshMenuState()
	t.relayoutMenu()
}

// toggleExitNode uses append=true: append=false would drop the whole current
// selection (default-on semantics), turning off every other routed network the
// user had enabled. Mutual exclusion of exit nodes is enforced daemon-side.
func (t *Tray) toggleExitNode(id string, selected bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		params := services.SelectNetworksParams{NetworkIDs: []string{id}, Append: true, All: false}
		var err error
		if selected {
			err = t.svc.Networks.Deselect(ctx, params)
		} else {
			err = t.svc.Networks.Select(ctx, params)
		}
		if err != nil {
			log.Errorf("tray toggle exit node %q: %v", id, err)
			t.notifyError(t.loc.T("notify.error.exitNode", "name", id))
			return
		}
		t.refreshExitNodes()
	}()
}

// exitNodesFromNetworks keeps only networks whose range is a default route: those are the exit-node candidates.
func exitNodesFromNetworks(networks []services.Network) []exitNodeEntry {
	out := []exitNodeEntry{}
	for _, n := range networks {
		if !rangeIsDefaultRoute(n.Range) {
			continue
		}
		out = append(out, exitNodeEntry{ID: n.ID, Selected: n.Selected})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].ID) < strings.ToLower(out[j].ID)
	})
	return out
}

// rangeIsDefaultRoute reports whether r contains a default route. The daemon may
// comma-join a v4+v6 pair ("0.0.0.0/0, ::/0"), so each part is parsed rather than string-compared.
func rangeIsDefaultRoute(r string) bool {
	for _, part := range strings.Split(r, ",") {
		pref, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		if pref.Bits() == 0 && pref.Addr().IsUnspecified() {
			return true
		}
	}
	return false
}

func equalExitNodes(a, b []exitNodeEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
