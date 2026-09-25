//go:build darwin && !ios

package systemops

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-multierror"
	log "github.com/sirupsen/logrus"

	nberrors "github.com/netbirdio/netbird/client/errors"
	"github.com/netbirdio/netbird/client/iface/wgaddr"
)

const (
	scutilPath    = "/usr/sbin/scutil"
	scutilTimeout = 5 * time.Second

	// OverlayServiceID is the configd service the overlay is published as while
	// it is the primary network service.
	OverlayServiceID = "NetBird-Overlay"

	overlayServiceKey     = "State:/Network/Service/" + OverlayServiceID
	overlayServiceIPv4Key = overlayServiceKey + "/IPv4"
	overlayServiceIPv6Key = overlayServiceKey + "/IPv6"
	overlayServiceDNSKey  = overlayServiceKey + "/DNS"

	globalIPv4Key = "State:/Network/Global/IPv4"
	globalIPv6Key = "State:/Network/Global/IPv6"
	globalDNSKey  = "State:/Network/Global/DNS"

	// overlaySettleWindow bounds how long configd keeps rewriting the routing
	// table after the overlay is promoted or withdrawn.
	overlaySettleWindow = 5 * time.Second

	defaultDNSPort = 53
)

// OverlayPrimaryState describes the overlay's standing as configd's primary
// network service. The network monitor and the DNS configurator read it to tell
// the route and resolver changes of a promotion apart from a change of the
// physical network.
type OverlayPrimaryState struct {
	// Active is true while the overlay service is published.
	Active bool
	// Interface is the overlay interface, V4 and V6 the addresses published on it.
	Interface *net.Interface
	V4, V6    netip.Addr
	// PhysicalV4 and PhysicalV6 are the physical default nexthops the overlay
	// displaces from the unscoped routing table.
	PhysicalV4, PhysicalV6 Nexthop
	// PhysicalService is the configd service that was primary before the overlay.
	PhysicalService string
	// Changed is when the overlay was last published or withdrawn.
	Changed time.Time
}

// Settling reports whether configd may still be rewriting routes after the
// last promotion or withdrawal.
func (s OverlayPrimaryState) Settling(now time.Time) bool {
	return !s.Changed.IsZero() && now.Sub(s.Changed) < overlaySettleWindow
}

// Physical returns the physical default nexthop of addr's address family.
func (s OverlayPrimaryState) Physical(addr netip.Addr) Nexthop {
	if addr.Is6() {
		return s.PhysicalV6
	}
	return s.PhysicalV4
}

// OverlayPrimary returns the current state of the overlay promotion.
func OverlayPrimary() OverlayPrimaryState {
	return overlayCtl.snapshot()
}

// SetOverlayResolver records the address NetBird serves host DNS on. As the
// primary network service the overlay becomes the system's default resolver,
// so it is only promoted while a resolver is set. owner must be comparable,
// and only the same owner can clear it again.
func SetOverlayResolver(owner any, addr netip.AddrPort) error {
	return overlayCtl.setResolver(owner, addr)
}

// ClearOverlayResolver drops the resolver set by owner, withdrawing the
// overlay if it was promoted.
func ClearOverlayResolver(owner any) error {
	return overlayCtl.clearResolver(owner)
}

// setOverlayDefault tells the overlay controller whether prefix, 0.0.0.0/0 or
// ::/0, is routed through the overlay. Routing works either way, only AAAA
// resolution depends on the promotion, so failures are logged.
func (r *SysOps) setOverlayDefault(prefix netip.Prefix, intf *net.Interface, routed bool) {
	if r.wgInterface == nil || intf == nil {
		return
	}

	r.physicalDefaults.mu.Lock()
	d := overlayDefaults{
		owner:      r,
		intf:       intf,
		addr:       r.wgInterface.Address(),
		physicalV4: r.physicalDefaults.v4,
		physicalV6: r.physicalDefaults.v6,
	}
	r.physicalDefaults.mu.Unlock()

	if err := overlayCtl.setDefault(d, prefix, routed); err != nil {
		log.Warnf("failed to update the overlay primary network service: %v", err)
	}
}

// removeOverlayService removes the overlay service, including one a crashed
// daemon left behind. Removing keys that do not exist is not an error.
func removeOverlayService() error {
	return overlayCtl.removeAll()
}

// overlayDefaults are the default routes one SysOps sends through the overlay.
type overlayDefaults struct {
	owner      *SysOps
	intf       *net.Interface
	addr       wgaddr.Address
	physicalV4 Nexthop
	physicalV6 Nexthop
	v4, v6     bool
}

// overlayService is what gets published to configd.
type overlayService struct {
	ifName        string
	v4, v6        netip.Prefix
	resolver      netip.AddrPort
	searchDomains []string
	domainName    string
}

func (s overlayService) sameEndpoints(o overlayService) bool {
	return s.ifName == o.ifName && s.v4 == o.v4 && s.v6 == o.v6 && s.resolver == o.resolver
}

// overlayController promotes the overlay to configd's primary network service
// while an exit node carries both address families and the physical network has
// no IPv6 of its own.
//
// configd sets the resolver's "Request AAAA records" flag only for services that
// are routable for IPv6 after primary election (IPMonitor dns-configuration.c).
// NetBird's split defaults live in the routing table only, so on an IPv4-only
// network macOS stops asking for AAAA records although the tunnel carries IPv6.
// Ranked First, like a VPN with OverridePrimary, the overlay takes over both
// families while the physical interface keeps a scoped default for the sockets
// NetBird binds to it. An overlay without IPv4 cannot be ranked First: configd
// would drop the physical IPv4 default without a replacement.
type overlayController struct {
	mu  sync.Mutex
	run func(commands string) (string, error)

	defaults      *overlayDefaults
	resolver      netip.AddrPort
	resolverOwner any
	// published is what configd holds, nil while the overlay is withdrawn.
	published *overlayService

	state atomic.Pointer[OverlayPrimaryState]
}

var overlayCtl = &overlayController{run: runScutil}

func (c *overlayController) snapshot() OverlayPrimaryState {
	if s := c.state.Load(); s != nil {
		return *s
	}
	return OverlayPrimaryState{}
}

func (c *overlayController) storeState(s OverlayPrimaryState) {
	c.state.Store(&s)
}

func (c *overlayController) markWithdrawn() {
	s := c.snapshot()
	s.Active = false
	s.Changed = time.Now()
	c.storeState(s)
}

// setDefault records whether prefix, 0.0.0.0/0 or ::/0, is routed through the
// overlay by d.owner, then promotes or withdraws the overlay to match.
func (c *overlayController) setDefault(d overlayDefaults, prefix netip.Prefix, routed bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	cur := c.defaults
	if cur == nil || cur.owner != d.owner {
		// An engine still tearing down after a newer one took over must leave
		// the newer one's promotion alone.
		if !routed {
			return nil
		}
		cur = &overlayDefaults{owner: d.owner}
		c.defaults = cur
	}

	cur.intf, cur.addr = d.intf, d.addr
	cur.physicalV4, cur.physicalV6 = d.physicalV4, d.physicalV6
	if prefix.Addr().Is4() {
		cur.v4 = routed
	} else {
		cur.v6 = routed
	}
	return c.reconcileLocked()
}

func (c *overlayController) setResolver(owner any, addr netip.AddrPort) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.resolver, c.resolverOwner = addr, owner
	return c.reconcileLocked()
}

func (c *overlayController) clearResolver(owner any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.resolverOwner != owner {
		return nil
	}
	c.resolver, c.resolverOwner = netip.AddrPort{}, nil
	return c.reconcileLocked()
}

func (c *overlayController) removeAll() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.published != nil {
		c.markWithdrawn()
	}
	if err := c.remove(); err != nil {
		return err
	}
	c.published = nil
	return nil
}

// wantedLocked returns the service the overlay should be published as, or why
// it should not be published.
func (c *overlayController) wantedLocked() (*overlayService, string) {
	d := c.defaults
	switch {
	case d == nil || !d.v4 || !d.v6:
		return nil, "0.0.0.0/0 and ::/0 are not both routed through the overlay"
	case !d.addr.IP.IsValid() || !d.addr.HasIPv6():
		return nil, "the overlay lacks an IPv4 or IPv6 address"
	case d.physicalV4.Intf == nil:
		return nil, "no scoped physical default keeps NetBird's own sockets off the tunnel"
	case !c.resolver.IsValid():
		return nil, "NetBird does not serve the host DNS"
	}

	return &overlayService{
		ifName:   d.intf.Name,
		v4:       d.addr.Prefix(),
		v6:       d.addr.IPv6Prefix(),
		resolver: c.resolver,
	}, ""
}

func (c *overlayController) reconcileLocked() error {
	want, reason := c.wantedLocked()
	switch {
	case want == nil && c.published == nil:
		log.Debugf("not promoting the overlay to primary network service: %s", reason)
		return nil
	case want == nil:
		log.Infof("withdrawing the overlay from primary network service: %s", reason)
		return c.withdrawLocked()
	case c.published == nil:
		return c.promoteLocked(*want)
	case !c.published.sameEndpoints(*want):
		return c.republishLocked(*want)
	}
	return nil
}

func (c *overlayController) promoteLocked(svc overlayService) error {
	globals, err := c.show(globalIPv4Key, globalIPv6Key, globalDNSKey)
	if err != nil {
		return fmt.Errorf("read primary services: %w", err)
	}
	global4, global6, globalDNS := globals[0], globals[1], globals[2]

	if iface, ok := physicalIPv6Primary(global6, svc.ifName); ok {
		log.Infof("not promoting %s to primary network service: %s routes IPv6 natively", svc.ifName, iface)
		return nil
	}

	// The overlay's DNS entity replaces the physical one as the default
	// resolver, so it carries the physical search list over.
	svc.searchDomains = validSearchDomains(globalDNS.arrays["SearchDomains"])
	if name := globalDNS.values["DomainName"]; validSearchDomain(name) {
		svc.domainName = name
	}

	physicalService := global4.values["PrimaryService"]
	if physicalService == OverlayServiceID {
		physicalService = c.snapshot().PhysicalService
	}

	// Store the state before configd reacts, so the network monitor recognizes
	// the default routes the promotion is about to move.
	d := c.defaults
	c.storeState(OverlayPrimaryState{
		Active:          true,
		Interface:       d.intf,
		V4:              svc.v4.Addr(),
		V6:              svc.v6.Addr(),
		PhysicalV4:      d.physicalV4,
		PhysicalV6:      d.physicalV6,
		PhysicalService: physicalService,
		Changed:         time.Now(),
	})

	if err := c.publish(svc); err != nil {
		c.markWithdrawn()
		merr := multierror.Append(nil, err)
		if err := c.remove(); err != nil {
			merr = multierror.Append(merr, fmt.Errorf("roll back: %w", err))
		}
		return nberrors.FormatErrorOrNil(merr)
	}

	c.published = &svc
	log.Infof("promoted %s to primary network service so the system resolver requests AAAA records", svc.ifName)
	return nil
}

func (c *overlayController) republishLocked(svc overlayService) error {
	svc.searchDomains, svc.domainName = c.published.searchDomains, c.published.domainName

	s := c.snapshot()
	s.Interface = c.defaults.intf
	s.V4, s.V6 = svc.v4.Addr(), svc.v6.Addr()
	s.Changed = time.Now()
	c.storeState(s)

	if err := c.publish(svc); err != nil {
		merr := multierror.Append(nil, err)
		if err := c.withdrawLocked(); err != nil {
			merr = multierror.Append(merr, err)
		}
		return nberrors.FormatErrorOrNil(merr)
	}

	c.published = &svc
	log.Infof("republished the overlay primary network service on %s", svc.ifName)
	return nil
}

func (c *overlayController) withdrawLocked() error {
	c.markWithdrawn()
	if err := c.remove(); err != nil {
		return err
	}
	c.published = nil
	log.Infof("withdrew the overlay from primary network service")
	return nil
}

// publish writes svc to configd and reads it back, because scutil exits 0 even
// when one of its commands fails.
func (c *overlayController) publish(svc overlayService) error {
	if _, err := c.run(buildOverlayServiceCommands(svc)); err != nil {
		return fmt.Errorf("publish overlay service: %w", err)
	}

	checks := []struct{ key, field string }{
		{overlayServiceKey, "PrimaryRank"},
		{overlayServiceDNSKey, "ServerAddresses"},
		{overlayServiceIPv6Key, "Router"},
		{overlayServiceIPv4Key, "Router"},
	}
	keys := make([]string, 0, len(checks))
	for _, check := range checks {
		keys = append(keys, check.key)
	}

	got, err := c.show(keys...)
	if err != nil {
		return fmt.Errorf("read back overlay service: %w", err)
	}
	for i, check := range checks {
		if !got[i].has(check.field) {
			return fmt.Errorf("configd has no %s in %s after publishing", check.field, check.key)
		}
	}
	return nil
}

func (c *overlayController) remove() error {
	if _, err := c.run(buildOverlayWithdrawCommands()); err != nil {
		return fmt.Errorf("remove overlay service: %w", err)
	}
	return nil
}

// show reads keys from configd in a single scutil session.
func (c *overlayController) show(keys ...string) ([]scutilDict, error) {
	var b strings.Builder
	b.WriteString("open\n")
	for _, key := range keys {
		fmt.Fprintf(&b, "show %s\n", key)
	}
	b.WriteString("quit\n")

	out, err := c.run(b.String())
	if err != nil {
		return nil, err
	}

	dicts := parseScutilShows(out)
	if len(dicts) != len(keys) {
		return nil, fmt.Errorf("expected %d results from scutil, got %d: %s", len(keys), len(dicts), strings.TrimSpace(out))
	}
	return dicts, nil
}

// physicalIPv6Primary returns the interface of configd's IPv6 primary when it
// is not the overlay, that is when the physical network routes IPv6 itself.
func physicalIPv6Primary(global6 scutilDict, overlayIf string) (string, bool) {
	if !global6.found {
		return "", false
	}
	iface := global6.values["PrimaryInterface"]
	if global6.values["PrimaryService"] == OverlayServiceID || iface == overlayIf {
		return "", false
	}
	return iface, true
}

// buildOverlayServiceCommands returns the scutil script that publishes svc. The
// rank goes first so configd never elects the service at the default rank, and
// DNS before the address entities so the system keeps a default resolver once
// the overlay is elected. The router is the overlay address itself, which makes
// the defaults on-link on the point-to-point interface.
func buildOverlayServiceCommands(svc overlayService) string {
	var b strings.Builder
	b.WriteString("open\n")

	b.WriteString("d.init\n")
	b.WriteString("d.add PrimaryRank First\n")
	fmt.Fprintf(&b, "set %s\n", overlayServiceKey)

	b.WriteString("d.init\n")
	fmt.Fprintf(&b, "d.add ServerAddresses * %s\n", svc.resolver.Addr())
	if svc.resolver.Port() != defaultDNSPort {
		fmt.Fprintf(&b, "d.add ServerPort # %d\n", svc.resolver.Port())
	}
	if len(svc.searchDomains) > 0 {
		fmt.Fprintf(&b, "d.add SearchDomains * %s\n", strings.Join(svc.searchDomains, " "))
	}
	if svc.domainName != "" {
		fmt.Fprintf(&b, "d.add DomainName %s\n", svc.domainName)
	}
	fmt.Fprintf(&b, "set %s\n", overlayServiceDNSKey)

	v6 := svc.v6.Addr()
	b.WriteString("d.init\n")
	fmt.Fprintf(&b, "d.add InterfaceName %s\n", svc.ifName)
	fmt.Fprintf(&b, "d.add Addresses * %s\n", v6)
	fmt.Fprintf(&b, "d.add PrefixLength * # %d\n", svc.v6.Bits())
	fmt.Fprintf(&b, "d.add Router %s\n", v6)
	fmt.Fprintf(&b, "set %s\n", overlayServiceIPv6Key)

	v4 := svc.v4.Addr()
	b.WriteString("d.init\n")
	fmt.Fprintf(&b, "d.add InterfaceName %s\n", svc.ifName)
	fmt.Fprintf(&b, "d.add Addresses * %s\n", v4)
	fmt.Fprintf(&b, "d.add SubnetMasks * %s\n", net.IP(net.CIDRMask(svc.v4.Bits(), 32)))
	fmt.Fprintf(&b, "d.add Router %s\n", v4)
	fmt.Fprintf(&b, "set %s\n", overlayServiceIPv4Key)

	b.WriteString("quit\n")
	return b.String()
}

// buildOverlayWithdrawCommands returns the scutil script that removes the
// overlay service in the reverse order of publishing, so configd hands the
// defaults back to the physical service before the overlay's resolver goes.
func buildOverlayWithdrawCommands() string {
	var b strings.Builder
	b.WriteString("open\n")
	for _, key := range []string{overlayServiceIPv4Key, overlayServiceIPv6Key, overlayServiceDNSKey, overlayServiceKey} {
		fmt.Fprintf(&b, "remove %s\n", key)
	}
	b.WriteString("quit\n")
	return b.String()
}

// validSearchDomains keeps the domains that are safe to write into a scutil
// script. They come from DHCP, and a stray space or newline would change the
// script NetBird runs as root.
func validSearchDomains(domains []string) []string {
	var valid []string
	for _, d := range domains {
		if validSearchDomain(d) {
			valid = append(valid, d)
			continue
		}
		log.Debugf("not carrying over search domain %q", d)
	}
	return valid
}

func validSearchDomain(d string) bool {
	if d == "" || len(d) > 253 {
		return false
	}
	for _, r := range d {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return true
}

// scutilDict holds the scalar and array values of a dictionary printed by
// scutil's show command. Nested dictionaries are skipped.
type scutilDict struct {
	found  bool
	values map[string]string
	arrays map[string][]string
}

func (d scutilDict) has(key string) bool {
	if _, ok := d.values[key]; ok {
		return true
	}
	_, ok := d.arrays[key]
	return ok
}

// parseScutilShows splits the output of consecutive show commands into one
// dictionary per command, in order. A missing key yields a dictionary that is
// not found.
func parseScutilShows(out string) []scutilDict {
	var (
		dicts []scutilDict
		cur   scutilDict
		depth int
		array string
	)

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case depth == 0 && line == "No such key":
			dicts = append(dicts, scutilDict{})
		case depth == 0 && strings.HasSuffix(line, "<dictionary> {"):
			cur = scutilDict{found: true, values: map[string]string{}, arrays: map[string][]string{}}
			depth = 1
		case depth == 0:
			continue
		case line == "}":
			depth--
			if depth < 2 {
				array = ""
			}
			if depth == 0 {
				dicts = append(dicts, cur)
			}
		case strings.HasSuffix(line, "{"):
			depth++
			if depth == 2 && strings.HasSuffix(line, "<array> {") {
				array, _, _ = strings.Cut(line, " : ")
				cur.arrays[array] = []string{}
			}
		default:
			key, value, ok := strings.Cut(line, " : ")
			if !ok {
				continue
			}
			switch {
			case depth == 1:
				cur.values[key] = value
			case depth == 2 && array != "":
				cur.arrays[array] = append(cur.arrays[array], value)
			}
		}
	}
	return dicts
}

func runScutil(commands string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scutilTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, scutilPath)
	cmd.Stdin = strings.NewReader(commands)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run scutil: %w, output: %s", err, out)
	}
	return string(out), nil
}
