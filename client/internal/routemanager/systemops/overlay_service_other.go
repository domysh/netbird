//go:build !darwin || ios

package systemops

import "net"

// announceV6Default is a no-op outside macOS, where resolvers do not gate AAAA
// queries on a system-level IPv6 service.
func (r *SysOps) announceV6Default(*net.Interface) error {
	return nil
}

// withdrawV6Default is a no-op outside macOS.
func (r *SysOps) withdrawV6Default() error {
	return nil
}
