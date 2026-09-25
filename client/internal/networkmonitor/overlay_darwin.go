//go:build darwin && !ios

package networkmonitor

import (
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/netbirdio/netbird/client/internal/routemanager/systemops"
)

type routeVerdict int

const (
	// verdictUnrelated leaves the event to the generic default route rules.
	verdictUnrelated routeVerdict = iota
	verdictIgnore
	verdictChange
)

// isNetworkChange reports whether a default route event is a network change,
// telling the route churn of the overlay's promotion to primary network service
// apart from a change of the physical network.
func isNetworkChange(ev defaultRouteEvent, nexthopv4, nexthopv6 systemops.Nexthop, overlay systemops.OverlayPrimaryState, now time.Time) bool {
	verdict, reason := overlayRouteVerdict(ev, overlay, now)
	switch verdict {
	case verdictIgnore:
		log.Debugf("Network monitor: ignoring default route %s: %s", ev, reason)
		return false
	case verdictChange:
		log.Infof("Network monitor: %s: %s", reason, ev)
		return true
	default:
		return defaultRouteChanged(ev, nexthopv4, nexthopv6)
	}
}

// overlayRouteVerdict classifies ev against the overlay's promotion. configd
// moves the unscoped defaults between the physical interface and the overlay
// when the overlay is promoted or withdrawn, which must not restart the engine.
// While the overlay is primary the physical interface keeps only a scoped
// default, so losing that one, or the physical interface gaining an IPv6
// default, is the network change to act on.
func overlayRouteVerdict(ev defaultRouteEvent, overlay systemops.OverlayPrimaryState, now time.Time) (routeVerdict, string) {
	settling := overlay.Settling(now)
	if !overlay.Active && !settling {
		return verdictUnrelated, ""
	}

	if onOverlay(ev, overlay) {
		return verdictIgnore, "overlay default moved by the overlay promotion"
	}

	scoped := ev.flags&unix.RTF_IFSCOPE != 0
	physical := overlay.Physical(ev.dst.Addr())
	if viaPhysicalGateway(ev, physical) {
		switch {
		case !scoped:
			return verdictIgnore, "physical default moved by the overlay promotion"
		case settling:
			return verdictIgnore, "physical scoped default rewritten by the overlay promotion"
		case ev.msgType == unix.RTM_DELETE:
			return verdictChange, "physical default removed"
		}
		return verdictUnrelated, ""
	}

	if !overlay.Active || ev.msgType != unix.RTM_ADD || !scoped || !onInterface(ev, overlay.PhysicalV4.Intf) {
		return verdictUnrelated, ""
	}
	switch {
	case ev.dst.Addr().Is6():
		return verdictChange, "IPv6 default appeared on the physical interface"
	case !settling && ev.gw.IsValid():
		return verdictChange, "physical interface got a new default gateway"
	}
	return verdictUnrelated, ""
}

// physicalNexthops swaps a default nexthop captured on the overlay for the
// physical one the promotion displaced, so that losing the physical default is
// still recognized when monitoring started after the promotion.
func physicalNexthops(nexthopv4, nexthopv6 systemops.Nexthop, overlay systemops.OverlayPrimaryState) (systemops.Nexthop, systemops.Nexthop) {
	if !overlay.Active || overlay.Interface == nil {
		return nexthopv4, nexthopv6
	}
	if nexthopOnInterface(nexthopv4, overlay.Interface) {
		nexthopv4 = overlay.PhysicalV4
	}
	if nexthopOnInterface(nexthopv6, overlay.Interface) {
		nexthopv6 = overlay.PhysicalV6
	}
	return nexthopv4, nexthopv6
}

func onOverlay(ev defaultRouteEvent, overlay systemops.OverlayPrimaryState) bool {
	if onInterface(ev, overlay.Interface) {
		return true
	}
	gw := ev.gw.WithZone("")
	return gw.IsValid() && (gw == overlay.V4 || gw == overlay.V6)
}

func onInterface(ev defaultRouteEvent, intf *net.Interface) bool {
	if intf == nil {
		return false
	}
	return ev.ifIndex != 0 && ev.ifIndex == intf.Index || ev.ifName != "" && ev.ifName == intf.Name
}

func viaPhysicalGateway(ev defaultRouteEvent, physical systemops.Nexthop) bool {
	if physical.Intf == nil || !physical.IP.IsValid() {
		return false
	}
	return ev.gw.WithZone("") == physical.IP.WithZone("")
}

func nexthopOnInterface(nexthop systemops.Nexthop, intf *net.Interface) bool {
	return nexthop.Intf != nil && (nexthop.Intf.Index == intf.Index || nexthop.Intf.Name == intf.Name)
}
