//go:build !android && !ios && !freebsd && !js

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A native click on an exit-node row reports its position, and nothing else:
// the row order the painter is handed is the only thing that maps it back to a
// network, so it has to be the order the rows were rendered in.
func TestExitNodeLabelsKeepRowOrder(t *testing.T) {
	nodes := []exitNodeEntry{
		{ID: "alpha"},
		{ID: "bravo", Selected: true},
		{ID: "charlie"},
	}

	labels := exitNodeLabels(nodes)

	require.Len(t, labels, len(nodes), "one label per node")
	assert.Equal(t, []string{"alpha", "✓ bravo", "charlie"}, labels,
		"labels must stay in the order the nodes were given in")
}

// The selected row is marked with a prefix rather than a checkbox: Wails
// auto-toggles a checkbox before OnClick runs, so a deselect/select round-trip
// would briefly show two checked rows.
func TestExitNodeRowLabelMarksSelection(t *testing.T) {
	assert.Equal(t, "alpha", exitNodeRowLabel(exitNodeEntry{ID: "alpha"}))
	assert.Equal(t, "✓ alpha", exitNodeRowLabel(exitNodeEntry{ID: "alpha", Selected: true}))
}

// Both painters address a row by its menuRow, so the markers that let the
// AppKit one find an NSMenuItem must stay distinct — and none may be a suffix
// of another, or a lookup lands on the wrong row.
func TestRowMarkersAreDistinct(t *testing.T) {
	rows := []menuRow{rowConnection, rowStatus, rowSession, rowExitNode, rowSettings, rowProfiles}

	seen := make(map[string]menuRow, len(rows))
	for _, row := range rows {
		marker := rowMarker(row)
		if marker == "" {
			continue // platforms without the AppKit painter carry no markers
		}
		previous, clash := seen[marker]
		require.False(t, clash, "rows %d and %d share a marker", previous, row)
		seen[marker] = row

		for other := range seen {
			if other == marker {
				continue
			}
			assert.False(t, strings.HasSuffix(marker, other), "marker of row %d ends with another row's", row)
			assert.False(t, strings.HasSuffix(other, marker), "another row's marker ends with row %d's", row)
		}
	}
}
