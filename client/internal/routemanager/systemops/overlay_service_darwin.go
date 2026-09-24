//go:build darwin && !ios

package systemops

import (
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
)

const (
	scutilPath = "/usr/sbin/scutil"

	// overlayServiceKey holds the service options of the overlay service and
	// overlayServiceIPv6Key its IPv6 entity. Both live in the State: domain,
	// which configd merges with the services defined in Setup:.
	overlayServiceKey     = "State:/Network/Service/NetBird-Overlay"
	overlayServiceIPv6Key = overlayServiceKey + "/IPv6"

	// overlayPrimaryRank keeps a physical IPv6 service primary when there is
	// one, so the overlay only becomes the IPv6 primary where it is the sole
	// IPv6 path.
	overlayPrimaryRank = "Last"
)

// announceV6Default publishes the overlay to configd as an IPv6 service with a
// default route while an exit node carries ::/0.
//
// configd sets the resolver's "Request AAAA records" flag only when some
// service has an IPv6 entity with a default route (IPMonitor
// dns-configuration.c, dns_resolver_flags_service). NetBird's split default
// lives in the routing table only, so on an IPv4-only network macOS stops
// asking for AAAA records even though the tunnel carries IPv6.
func (r *SysOps) announceV6Default(intf *net.Interface) error {
	if r.wgInterface == nil || intf == nil {
		return nil
	}

	addr := r.wgInterface.Address()
	if !addr.HasIPv6() {
		return nil
	}

	if err := runScutil(buildOverlayV6ServiceCommands(intf.Name, addr.IPv6Prefix())); err != nil {
		return fmt.Errorf("publish overlay IPv6 service: %w", err)
	}
	return nil
}

// withdrawV6Default removes the overlay IPv6 service published by
// announceV6Default. Removing keys that do not exist is not an error, so it is
// safe to call at startup to clear what a crashed daemon left behind.
func (r *SysOps) withdrawV6Default() error {
	if err := runScutil(buildOverlayV6WithdrawCommands()); err != nil {
		return fmt.Errorf("remove overlay IPv6 service: %w", err)
	}
	return nil
}

// buildOverlayV6ServiceCommands returns the scutil script that publishes the
// overlay IPv6 entity. The router is the overlay address itself: configd then
// treats the default route as on-link, which matches a point-to-point utun.
// The rank is written first so configd never elects the entity without it.
func buildOverlayV6ServiceCommands(ifName string, prefix netip.Prefix) string {
	addr := prefix.Addr().String()

	var b strings.Builder
	b.WriteString("open\n")
	b.WriteString("d.init\n")
	fmt.Fprintf(&b, "d.add PrimaryRank %s\n", overlayPrimaryRank)
	fmt.Fprintf(&b, "set %s\n", overlayServiceKey)
	b.WriteString("d.init\n")
	fmt.Fprintf(&b, "d.add InterfaceName %s\n", ifName)
	fmt.Fprintf(&b, "d.add Addresses * %s\n", addr)
	fmt.Fprintf(&b, "d.add PrefixLength * # %d\n", prefix.Bits())
	fmt.Fprintf(&b, "d.add Router %s\n", addr)
	fmt.Fprintf(&b, "set %s\n", overlayServiceIPv6Key)
	b.WriteString("quit\n")
	return b.String()
}

// buildOverlayV6WithdrawCommands returns the scutil script that removes the
// overlay IPv6 entity before the service options that rank it.
func buildOverlayV6WithdrawCommands() string {
	return fmt.Sprintf("open\nremove %s\nremove %s\nquit\n", overlayServiceIPv6Key, overlayServiceKey)
}

func runScutil(commands string) error {
	cmd := exec.Command(scutilPath)
	cmd.Stdin = strings.NewReader(commands)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("run scutil: %w, output: %s", err, out)
	}
	return nil
}
