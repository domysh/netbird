//go:build !android && !ios && !freebsd && !js

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A click on a natively painted exit-node row reports its position and nothing
// else, so the order the painter is handed is what maps it back to a network.
func TestExitNodeLabelsKeepRowOrder(t *testing.T) {
	nodes := []exitNodeEntry{
		{ID: "alpha"},
		{ID: "bravo", Selected: true},
		{ID: "charlie"},
	}

	labels := exitNodeLabels(nodes)

	require.Len(t, labels, len(nodes), "one label per node")
	assert.Equal(t, []string{"alpha", "✓ bravo", "charlie"}, labels)
}

// The AppKit painter finds a row by matching its marker as a suffix, so a
// marker that is a suffix of another would paint the wrong row.
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

		for other := range seen {
			assert.False(t, strings.HasSuffix(marker, other), "row %d ends with another row's marker", row)
			assert.False(t, strings.HasSuffix(other, marker), "another row ends with row %d's marker", row)
		}
		seen[marker] = row
	}
}
