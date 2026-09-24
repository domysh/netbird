//go:build darwin && !ios

package systemops

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildOverlayV6ServiceCommands(t *testing.T) {
	// The rank must be set before the IPv6 entity so configd never elects the
	// overlay at the default rank and demotes it, and the router is the overlay address so the
	// default route is on-link for the point-to-point utun.
	got := buildOverlayV6ServiceCommands("utun100", netip.MustParsePrefix("fd00:1234::5/64"))

	want := "open\n" +
		"d.init\n" +
		"d.add PrimaryRank First\n" +
		"set State:/Network/Service/NetBird-Overlay\n" +
		"d.init\n" +
		"d.add InterfaceName utun100\n" +
		"d.add Addresses * fd00:1234::5\n" +
		"d.add PrefixLength * # 64\n" +
		"d.add Router fd00:1234::5\n" +
		"set State:/Network/Service/NetBird-Overlay/IPv6\n" +
		"quit\n"

	assert.Equal(t, want, got, "scutil script should publish the rank, then the IPv6 entity")
}

func TestBuildOverlayV6WithdrawCommands(t *testing.T) {
	want := "open\n" +
		"remove State:/Network/Service/NetBird-Overlay/IPv6\n" +
		"remove State:/Network/Service/NetBird-Overlay\n" +
		"quit\n"

	assert.Equal(t, want, buildOverlayV6WithdrawCommands(), "withdraw should remove the entity before its rank")
}
