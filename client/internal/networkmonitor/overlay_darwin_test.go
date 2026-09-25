//go:build darwin && !ios

package networkmonitor

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"

	"github.com/netbirdio/netbird/client/internal/routemanager/systemops"
)

var (
	testEn0     = &net.Interface{Index: 12, Name: "en0"}
	testUtun    = &net.Interface{Index: 23, Name: "utun100"}
	testPhysGw  = netip.MustParseAddr("10.251.254.5")
	testOverlay = systemops.OverlayPrimaryState{
		Interface:       testUtun,
		V4:              netip.MustParseAddr("100.91.146.163"),
		V6:              netip.MustParseAddr("fd84:622:a6ea:4a41:969b:f793:4860:d14a"),
		PhysicalV4:      systemops.Nexthop{IP: testPhysGw, Intf: testEn0},
		PhysicalService: "PHYS-SERVICE",
	}
	testNexthopV4 = systemops.Nexthop{IP: testPhysGw, Intf: testEn0}
)

const (
	unscoped = unix.RTF_UP | unix.RTF_GATEWAY | unix.RTF_STATIC
	scoped   = unscoped | unix.RTF_IFSCOPE
)

func TestIsNetworkChangeWithOverlay(t *testing.T) {
	now := time.Now()

	promoting := testOverlay
	promoting.Active, promoting.Changed = true, now
	promoted := testOverlay
	promoted.Active, promoted.Changed = true, now.Add(-time.Minute)
	withdrawing := testOverlay
	withdrawing.Changed = now
	withdrawn := testOverlay
	withdrawn.Changed = now.Add(-time.Minute)

	v4Default := netip.MustParsePrefix("0.0.0.0/0")
	v6Default := netip.MustParsePrefix("::/0")

	tests := []struct {
		name    string
		overlay systemops.OverlayPrimaryState
		ev      defaultRouteEvent
		want    bool
	}{
		// configd demotes en0 and installs the overlay defaults.
		{
			name:    "promotion removes the unscoped physical default",
			overlay: promoting,
			ev:      defaultRouteEvent{msgType: unix.RTM_DELETE, flags: unscoped, dst: v4Default, gw: testPhysGw},
			want:    false,
		},
		{
			name:    "promotion adds the scoped physical default",
			overlay: promoting,
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: scoped, dst: v4Default, gw: testPhysGw, ifIndex: testEn0.Index},
			want:    false,
		},
		{
			name:    "promotion adds the IPv4 default on the overlay link",
			overlay: promoting,
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: unix.RTF_UP | unix.RTF_STATIC, dst: v4Default, ifIndex: testUtun.Index, ifName: "link#23"},
			want:    false,
		},
		{
			name:    "promotion adds the IPv6 default via the overlay address",
			overlay: promoting,
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: unscoped, dst: v6Default, gw: testOverlay.V6},
			want:    false,
		},
		{
			name:    "scoped physical default deleted while configd settles",
			overlay: promoting,
			ev:      defaultRouteEvent{msgType: unix.RTM_DELETE, flags: scoped, dst: v4Default, gw: testPhysGw, ifIndex: testEn0.Index},
			want:    false,
		},

		// Changes of the physical network while the overlay is primary.
		{
			name:    "scoped physical default deleted after the promotion settled",
			overlay: promoted,
			ev:      defaultRouteEvent{msgType: unix.RTM_DELETE, flags: scoped, dst: v4Default, gw: testPhysGw, ifIndex: testEn0.Index},
			want:    true,
		},
		{
			name:    "native IPv6 default appears on the physical interface",
			overlay: promoted,
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: scoped, dst: v6Default, gw: netip.MustParseAddr("fe80::1").WithZone("12"), ifIndex: testEn0.Index},
			want:    true,
		},
		{
			name:    "physical interface gets a new scoped gateway",
			overlay: promoted,
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: scoped, dst: v4Default, gw: netip.MustParseAddr("192.168.1.1"), ifIndex: testEn0.Index},
			want:    true,
		},
		{
			name:    "physical scoped link default while configd settles",
			overlay: promoting,
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: unix.RTF_UP | unix.RTF_STATIC | unix.RTF_IFSCOPE, dst: v4Default, ifIndex: testEn0.Index, ifName: "en0"},
			want:    false,
		},
		{
			name:    "unscoped default via another gateway",
			overlay: promoted,
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: unscoped, dst: v4Default, gw: netip.MustParseAddr("192.168.1.1")},
			want:    true,
		},
		{
			name:    "scoped default on an unrelated interface",
			overlay: promoted,
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: scoped, dst: v4Default, gw: netip.MustParseAddr("10.0.0.1"), ifIndex: 30},
			want:    false,
		},

		// configd hands the defaults back to en0.
		{
			name:    "withdrawal removes the overlay default",
			overlay: withdrawing,
			ev:      defaultRouteEvent{msgType: unix.RTM_DELETE, flags: unix.RTF_UP | unix.RTF_STATIC, dst: v4Default, ifIndex: testUtun.Index, ifName: "utun100"},
			want:    false,
		},
		{
			name:    "withdrawal removes the scoped physical default",
			overlay: withdrawing,
			ev:      defaultRouteEvent{msgType: unix.RTM_DELETE, flags: scoped, dst: v4Default, gw: testPhysGw, ifIndex: testEn0.Index},
			want:    false,
		},
		{
			name:    "withdrawal restores the unscoped physical default",
			overlay: withdrawing,
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: unscoped, dst: v4Default, gw: testPhysGw},
			want:    false,
		},

		// Without a promotion, the generic rules apply unchanged.
		{
			name:    "unscoped default added long after the withdrawal",
			overlay: withdrawn,
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: unscoped, dst: v4Default, gw: testPhysGw},
			want:    true,
		},
		{
			name:    "physical default removed without a promotion",
			overlay: systemops.OverlayPrimaryState{},
			ev:      defaultRouteEvent{msgType: unix.RTM_DELETE, flags: unscoped, dst: v4Default, gw: testPhysGw},
			want:    true,
		},
		{
			name:    "scoped default added without a promotion",
			overlay: systemops.OverlayPrimaryState{},
			ev:      defaultRouteEvent{msgType: unix.RTM_ADD, flags: scoped, dst: v4Default, gw: testPhysGw, ifIndex: testEn0.Index},
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isNetworkChange(tc.ev, testNexthopV4, systemops.Nexthop{}, tc.overlay, now)
			assert.Equal(t, tc.want, got, "network change verdict for %s", tc.ev)
		})
	}
}

func TestPhysicalNexthops(t *testing.T) {
	onOverlayV4 := systemops.Nexthop{IP: testOverlay.V4, Intf: testUtun}
	onOverlayV6 := systemops.Nexthop{IP: testOverlay.V6, Intf: testUtun}

	promoted := testOverlay
	promoted.Active = true

	v4, v6 := physicalNexthops(onOverlayV4, onOverlayV6, promoted)
	assert.Equal(t, testNexthopV4, v4, "monitoring started after the promotion should watch the physical IPv4 default")
	assert.Nil(t, v6.Intf, "without a physical IPv6 default there is none to watch")

	v4, _ = physicalNexthops(testNexthopV4, systemops.Nexthop{}, promoted)
	assert.Equal(t, testNexthopV4, v4, "a physical nexthop is kept")

	v4, _ = physicalNexthops(onOverlayV4, systemops.Nexthop{}, testOverlay)
	assert.Equal(t, onOverlayV4, v4, "nothing is swapped without an active promotion")
}
