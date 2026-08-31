//go:build !android && !ios && !freebsd && !js

package main

import (
	"testing"

	"github.com/netbirdio/netbird/client/ui/services"
)

// loadProfiles runs on every connect/disconnect, and a relayout there would
// discard a menu the user has open (see refreshMenuState), so the change
// detection guarding it has to see through the daemon's row order.
func TestEqualProfilesAfterSort(t *testing.T) {
	tests := []struct {
		name  string
		a     []services.Profile
		b     []services.Profile
		equal bool
	}{
		{
			name:  "both empty",
			equal: true,
		},
		{
			name:  "nil vs empty",
			b:     []services.Profile{},
			equal: true,
		},
		{
			name: "same rows in daemon-shuffled order",
			a: []services.Profile{
				{ID: "b", Name: "work", IsActive: true, Email: "a@example.com"},
				{ID: "a", Name: "home"},
			},
			b: []services.Profile{
				{ID: "a", Name: "home"},
				{ID: "b", Name: "work", IsActive: true, Email: "a@example.com"},
			},
			equal: true,
		},
		{
			name: "same name, different id",
			a:    []services.Profile{{ID: "a", Name: "work"}},
			b:    []services.Profile{{ID: "b", Name: "work"}},
		},
		{
			name: "active profile moved",
			a:    []services.Profile{{ID: "a", Name: "home", IsActive: true}, {ID: "b", Name: "work"}},
			b:    []services.Profile{{ID: "a", Name: "home"}, {ID: "b", Name: "work", IsActive: true}},
		},
		{
			name: "email filled in after login",
			a:    []services.Profile{{ID: "a", Name: "home"}},
			b:    []services.Profile{{ID: "a", Name: "home", Email: "a@example.com"}},
		},
		{
			name: "profile removed",
			a:    []services.Profile{{ID: "a", Name: "home"}, {ID: "b", Name: "work"}},
			b:    []services.Profile{{ID: "a", Name: "home"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sortProfiles(tc.a)
			sortProfiles(tc.b)
			if got := equalProfiles(tc.a, tc.b); got != tc.equal {
				t.Errorf("equalProfiles() = %v, want %v", got, tc.equal)
			}
		})
	}
}
