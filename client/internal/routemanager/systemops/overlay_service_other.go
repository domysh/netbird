//go:build !darwin || ios

package systemops

import (
	"net"
	"net/netip"
)

// setOverlayDefault is a no-op outside macOS, where resolvers do not gate AAAA
// queries on the primary network service.
func (r *SysOps) setOverlayDefault(netip.Prefix, *net.Interface, bool) {}
