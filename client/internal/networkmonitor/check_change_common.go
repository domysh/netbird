//go:build dragonfly || freebsd || netbsd || openbsd || darwin

package networkmonitor

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"unsafe"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"

	"github.com/netbirdio/netbird/client/internal/routemanager/systemops"
)

func prepareFd() (int, error) {
	return unix.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW, syscall.AF_UNSPEC)
}

// defaultRouteEvent is a default route added to or removed from the routing
// table, as read from the routing socket.
type defaultRouteEvent struct {
	msgType int
	flags   int
	dst     netip.Prefix
	gw      netip.Addr
	// ifIndex is the link of an interface route, or else the index the kernel
	// reported, which for an RTF_IFSCOPE route is its scope. Zero when unknown.
	ifIndex int
	ifName  string
}

func (e defaultRouteEvent) String() string {
	intf := e.ifName
	if intf == "" {
		intf = "<nil>"
	}
	return fmt.Sprintf("via %s, interface %s, flags %#x", e.gw, intf, e.flags)
}

// routeCheck returns once isChange reports a default route event as a network
// change.
func routeCheck(ctx context.Context, fd int, isChange func(defaultRouteEvent) bool) error {
	for {
		// Wait until fd is readable or context is cancelled, to avoid a busy-loop
		// when the routing socket returns EAGAIN (e.g. immediately after wakeup).
		if err := waitReadable(ctx, fd); err != nil {
			return err
		}

		buf := make([]byte, 2048)
		n, err := unix.Read(fd, buf)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EBADF) || errors.Is(err, unix.EINVAL) {
				return fmt.Errorf("routing socket closed: %w", err)
			}
			return fmt.Errorf("read routing socket: %w", err)
		}

		if n < unix.SizeofRtMsghdr {
			log.Debugf("Network monitor: read from routing socket returned less than expected: %d bytes", n)
			continue
		}

		msg := (*unix.RtMsghdr)(unsafe.Pointer(&buf[0]))

		switch msg.Type {
		// handle route changes
		case unix.RTM_ADD, syscall.RTM_DELETE:
			ev, err := parseRouteMessage(buf[:n])
			if err != nil {
				log.Debugf("Network monitor: error parsing routing message: %v", err)
				continue
			}

			if ev.dst.Bits() != 0 {
				continue
			}

			if isChange(ev) {
				return nil
			}
		}
	}
}

// defaultRouteChanged reports whether a default route event changes the way
// out of the host, compared to the default nexthops captured when monitoring
// started.
func defaultRouteChanged(ev defaultRouteEvent, nexthopv4, nexthopv6 systemops.Nexthop) bool {
	switch ev.msgType {
	case unix.RTM_ADD:
		if systemops.IgnoreAddedDefaultRoute(ev.flags) {
			log.Debugf("Network monitor: ignoring added default route %s", ev)
			return false
		}
		log.Infof("Network monitor: default route changed: %s", ev)
		return true
	case unix.RTM_DELETE:
		if nexthopv4.Intf != nil && ev.gw.Compare(nexthopv4.IP) == 0 || nexthopv6.Intf != nil && ev.gw.Compare(nexthopv6.IP) == 0 {
			log.Infof("Network monitor: default route removed: %s", ev)
			return true
		}
	}
	return false
}

func parseRouteMessage(buf []byte) (defaultRouteEvent, error) {
	msgs, err := route.ParseRIB(route.RIBTypeRoute, buf)
	if err != nil {
		return defaultRouteEvent{}, fmt.Errorf("parse RIB: %v", err)
	}

	if len(msgs) != 1 {
		return defaultRouteEvent{}, fmt.Errorf("unexpected RIB message msgs: %v", msgs)
	}

	msg, ok := msgs[0].(*route.RouteMessage)
	if !ok {
		return defaultRouteEvent{}, fmt.Errorf("unexpected RIB message type: %T", msgs[0])
	}

	r, err := systemops.MsgToRoute(msg)
	if err != nil {
		return defaultRouteEvent{}, err
	}

	ev := defaultRouteEvent{
		msgType: msg.Type,
		flags:   msg.Flags,
		dst:     r.Dst,
		gw:      r.Gw,
		ifIndex: msg.Index,
	}
	if r.Interface != nil {
		ev.ifIndex, ev.ifName = r.Interface.Index, r.Interface.Name
	}
	return ev, nil
}

// waitReadable blocks until fd has data to read, or ctx is cancelled.
func waitReadable(ctx context.Context, fd int) error {
	var fdset unix.FdSet
	if fd < 0 || fd/unix.NFDBITS >= len(fdset.Bits) {
		return fmt.Errorf("fd %d out of range for FdSet", fd)
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		fdset = unix.FdSet{}
		fdset.Set(fd)
		// Use a 1-second timeout so we can re-check ctx periodically.
		tv := unix.Timeval{Sec: 1}
		n, err := unix.Select(fd+1, &fdset, nil, nil, &tv)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("select on routing socket: %w", err)
		}
		if n > 0 {
			return nil
		}
		// timeout — loop back and re-check ctx
	}
}
