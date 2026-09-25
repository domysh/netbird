//go:build darwin && !ios

package systemops

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/netbirdio/netbird/client/iface/wgaddr"
)

func TestBuildOverlayServiceCommands(t *testing.T) {
	// The rank comes first so configd never elects the service at the default
	// rank, DNS before the address entities so the system keeps a default
	// resolver once the overlay is elected, and each router is the overlay
	// address itself so the defaults are on-link on the point-to-point utun.
	got := buildOverlayServiceCommands(overlayService{
		ifName:   "utun100",
		v4:       netip.MustParsePrefix("100.91.146.163/16"),
		v6:       netip.MustParsePrefix("fd00:1234::5/64"),
		resolver: netip.MustParseAddrPort("100.91.255.254:53"),
	})

	want := "open\n" +
		"d.init\n" +
		"d.add PrimaryRank First\n" +
		"set State:/Network/Service/NetBird-Overlay\n" +
		"d.init\n" +
		"d.add ServerAddresses * 100.91.255.254\n" +
		"set State:/Network/Service/NetBird-Overlay/DNS\n" +
		"d.init\n" +
		"d.add InterfaceName utun100\n" +
		"d.add Addresses * fd00:1234::5\n" +
		"d.add PrefixLength * # 64\n" +
		"d.add Router fd00:1234::5\n" +
		"set State:/Network/Service/NetBird-Overlay/IPv6\n" +
		"d.init\n" +
		"d.add InterfaceName utun100\n" +
		"d.add Addresses * 100.91.146.163\n" +
		"d.add SubnetMasks * 255.255.0.0\n" +
		"d.add Router 100.91.146.163\n" +
		"set State:/Network/Service/NetBird-Overlay/IPv4\n" +
		"quit\n"

	assert.Equal(t, want, got, "scutil script should publish the rank, DNS, IPv6 and IPv4 in that order")
}

func TestBuildOverlayServiceCommandsDNSOptions(t *testing.T) {
	got := buildOverlayServiceCommands(overlayService{
		ifName:        "utun100",
		v4:            netip.MustParsePrefix("100.64.0.1/10"),
		v6:            netip.MustParsePrefix("fd00::1/64"),
		resolver:      netip.MustParseAddrPort("100.127.255.254:5053"),
		searchDomains: []string{"lan", "corp.example"},
		domainName:    "lan",
	})

	assert.Contains(t, got, "d.add ServerAddresses * 100.127.255.254\n"+
		"d.add ServerPort # 5053\n"+
		"d.add SearchDomains * lan corp.example\n"+
		"d.add DomainName lan\n"+
		"set State:/Network/Service/NetBird-Overlay/DNS\n",
		"a non-default port and the physical search list should be published with the resolver")
	assert.Contains(t, got, "d.add SubnetMasks * 255.192.0.0\n", "subnet mask should follow the overlay prefix")
}

func TestBuildOverlayWithdrawCommands(t *testing.T) {
	want := "open\n" +
		"remove State:/Network/Service/NetBird-Overlay/IPv4\n" +
		"remove State:/Network/Service/NetBird-Overlay/IPv6\n" +
		"remove State:/Network/Service/NetBird-Overlay/DNS\n" +
		"remove State:/Network/Service/NetBird-Overlay\n" +
		"quit\n"

	assert.Equal(t, want, buildOverlayWithdrawCommands(), "withdraw should remove the entities in reverse order of publishing")
}

func TestParseScutilShows(t *testing.T) {
	out := "<dictionary> {\n" +
		"  PrimaryInterface : en0\n" +
		"  PrimaryService : 8C3D1F2A-0000-4000-8000-000000000001\n" +
		"  Router : 10.251.254.5\n" +
		"}\n" +
		"  No such key\n" +
		"<dictionary> {\n" +
		"  DomainName : lan\n" +
		"  SearchDomains : <array> {\n" +
		"    0 : lan\n" +
		"    1 : corp.example\n" +
		"  }\n" +
		"  ServerAddresses : <array> {\n" +
		"    0 : 10.251.254.5\n" +
		"  }\n" +
		"  Nested : <dictionary> {\n" +
		"    Inner : ignored\n" +
		"  }\n" +
		"  __IF_INDEX__ : 12\n" +
		"}\n"

	got := parseScutilShows(out)
	require.Len(t, got, 3, "one result per show command")

	assert.True(t, got[0].found, "first key exists")
	assert.Equal(t, "en0", got[0].values["PrimaryInterface"])
	assert.Equal(t, "10.251.254.5", got[0].values["Router"])

	assert.False(t, got[1].found, "second key is missing")

	assert.True(t, got[2].found, "third key exists")
	assert.Equal(t, "lan", got[2].values["DomainName"])
	assert.Equal(t, []string{"lan", "corp.example"}, got[2].arrays["SearchDomains"])
	assert.Equal(t, []string{"10.251.254.5"}, got[2].arrays["ServerAddresses"])
	assert.Equal(t, "12", got[2].values["__IF_INDEX__"], "values after a nested dictionary still belong to the outer one")
	assert.NotContains(t, got[2].values, "Inner", "nested dictionaries are skipped")
}

func TestValidSearchDomain(t *testing.T) {
	for _, d := range []string{"lan", "corp.example.", "a-b_c.example"} {
		assert.True(t, validSearchDomain(d), "%q should be accepted", d)
	}
	for _, d := range []string{"", "two words", "evil\nremove State:/Network/Global/IPv4", "tab\there", strings.Repeat("a", 254)} {
		assert.False(t, validSearchDomain(d), "%q should be rejected", d)
	}
}

func TestOverlayControllerPromotesWithBothDefaultsAndResolver(t *testing.T) {
	cd := newIPv4OnlyConfigd()
	c := &overlayController{run: cd.run}
	owner := &SysOps{}
	d := testOverlayDefaults(owner)

	require.NoError(t, c.setDefault(d, netip.MustParsePrefix("0.0.0.0/0"), true))
	require.NoError(t, c.setResolver(owner, netip.MustParseAddrPort("100.91.255.254:53")))
	assert.False(t, cd.has(overlayServiceKey), "the overlay needs ::/0 routed as well")
	assert.False(t, c.snapshot().Active)

	require.NoError(t, c.setDefault(d, netip.MustParsePrefix("::/0"), true))

	require.True(t, cd.has(overlayServiceKey), "the overlay should be published")
	assert.Equal(t, "First", cd.store[overlayServiceKey].values["PrimaryRank"])

	dns := cd.store[overlayServiceDNSKey]
	assert.Equal(t, []string{"100.91.255.254"}, dns.arrays["ServerAddresses"], "the default resolver is NetBird")
	assert.Equal(t, []string{"lan", "corp.example"}, dns.arrays["SearchDomains"], "the physical search list is kept")
	assert.Equal(t, "lan", dns.values["DomainName"])
	assert.NotContains(t, dns.values, "ServerPort", "port 53 is the default")

	v6 := cd.store[overlayServiceIPv6Key]
	assert.Equal(t, "utun100", v6.values["InterfaceName"])
	assert.Equal(t, []string{"fd84:622:a6ea:4a41:969b:f793:4860:d14a"}, v6.arrays["Addresses"])
	assert.Equal(t, []string{"64"}, v6.arrays["PrefixLength"])
	assert.Equal(t, "fd84:622:a6ea:4a41:969b:f793:4860:d14a", v6.values["Router"])

	v4 := cd.store[overlayServiceIPv4Key]
	assert.Equal(t, "utun100", v4.values["InterfaceName"])
	assert.Equal(t, []string{"100.91.146.163"}, v4.arrays["Addresses"])
	assert.Equal(t, []string{"255.255.0.0"}, v4.arrays["SubnetMasks"])
	assert.Equal(t, "100.91.146.163", v4.values["Router"])

	st := c.snapshot()
	assert.True(t, st.Active, "state should report the promotion")
	assert.True(t, st.Settling(time.Now()), "configd is still rewriting routes right after the promotion")
	assert.False(t, st.Settling(time.Now().Add(overlaySettleWindow)), "the settle window is bounded")
	assert.Equal(t, "PHYS-SERVICE", st.PhysicalService, "the displaced physical service is recorded")
	assert.Equal(t, d.physicalV4, st.PhysicalV4)
	assert.Equal(t, "utun100", st.Interface.Name)
	assert.Equal(t, netip.MustParseAddr("100.91.146.163"), st.V4)
}

func TestOverlayControllerSkipsNativeIPv6(t *testing.T) {
	cd := newIPv4OnlyConfigd()
	cd.store[globalIPv6Key] = fakeDict{values: map[string]string{
		"PrimaryInterface": "en0",
		"PrimaryService":   "PHYS-SERVICE",
	}}
	c := &overlayController{run: cd.run}
	owner := &SysOps{}

	promoteAll(t, c, owner, testOverlayDefaults(owner))

	assert.False(t, cd.has(overlayServiceKey), "a physical IPv6 primary already makes configd request AAAA records")
	assert.False(t, c.snapshot().Active)
}

func TestOverlayControllerKeepsPromotionWhenIPv6PrimaryIsOverlay(t *testing.T) {
	// A previous promotion left the overlay as configd's IPv6 primary, which
	// is not native IPv6.
	cd := newIPv4OnlyConfigd()
	cd.store[globalIPv6Key] = fakeDict{values: map[string]string{
		"PrimaryInterface": "utun100",
		"PrimaryService":   OverlayServiceID,
	}}
	c := &overlayController{run: cd.run}
	owner := &SysOps{}

	promoteAll(t, c, owner, testOverlayDefaults(owner))

	assert.True(t, cd.has(overlayServiceIPv4Key), "the overlay should be published")
}

func TestOverlayControllerWithdrawsWhenAnInputGoes(t *testing.T) {
	tests := []struct {
		name     string
		withdraw func(c *overlayController, owner *SysOps, d overlayDefaults) error
	}{
		{
			name: "::/0 removed",
			withdraw: func(c *overlayController, _ *SysOps, d overlayDefaults) error {
				return c.setDefault(d, netip.MustParsePrefix("::/0"), false)
			},
		},
		{
			name: "0.0.0.0/0 removed",
			withdraw: func(c *overlayController, _ *SysOps, d overlayDefaults) error {
				return c.setDefault(d, netip.MustParsePrefix("0.0.0.0/0"), false)
			},
		},
		{
			name: "resolver cleared",
			withdraw: func(c *overlayController, owner *SysOps, _ overlayDefaults) error {
				return c.clearResolver(owner)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cd := newIPv4OnlyConfigd()
			c := &overlayController{run: cd.run}
			owner := &SysOps{}
			d := testOverlayDefaults(owner)
			promoteAll(t, c, owner, d)
			require.True(t, cd.has(overlayServiceKey), "precondition: promoted")

			require.NoError(t, tc.withdraw(c, owner, d))

			for _, key := range []string{overlayServiceKey, overlayServiceDNSKey, overlayServiceIPv6Key, overlayServiceIPv4Key} {
				assert.False(t, cd.has(key), "%s should be removed", key)
			}
			st := c.snapshot()
			assert.False(t, st.Active, "state should report the withdrawal")
			assert.True(t, st.Settling(time.Now()), "configd is still restoring the physical defaults")
			assert.Equal(t, d.physicalV4, st.PhysicalV4, "the physical nexthop stays known while configd settles")
		})
	}
}

func TestOverlayControllerIgnoresOtherResolverOwner(t *testing.T) {
	cd := newIPv4OnlyConfigd()
	c := &overlayController{run: cd.run}
	owner := &SysOps{}
	promoteAll(t, c, owner, testOverlayDefaults(owner))

	require.NoError(t, c.clearResolver(&SysOps{}))

	assert.True(t, cd.has(overlayServiceKey), "only the owner may clear the resolver")
}

func TestOverlayControllerIgnoresStaleEngine(t *testing.T) {
	cd := newIPv4OnlyConfigd()
	c := &overlayController{run: cd.run}
	resolverOwner := &SysOps{}
	oldEngine, newEngine := &SysOps{}, &SysOps{}
	oldDefaults, newDefaults := testOverlayDefaults(oldEngine), testOverlayDefaults(newEngine)

	promoteAll(t, c, resolverOwner, oldDefaults)
	require.NoError(t, c.setDefault(newDefaults, netip.MustParsePrefix("0.0.0.0/0"), true))
	require.NoError(t, c.setDefault(newDefaults, netip.MustParsePrefix("::/0"), true))
	require.True(t, cd.has(overlayServiceKey), "precondition: promoted for the new engine")

	// The old engine finishes tearing down after the new one took over.
	require.NoError(t, c.setDefault(oldDefaults, netip.MustParsePrefix("::/0"), false))
	require.NoError(t, c.setDefault(oldDefaults, netip.MustParsePrefix("0.0.0.0/0"), false))

	assert.True(t, cd.has(overlayServiceKey), "a stale engine must not withdraw the new engine's promotion")
	assert.True(t, c.snapshot().Active)
}

func TestOverlayControllerRequiresScopedPhysicalDefault(t *testing.T) {
	cd := newIPv4OnlyConfigd()
	c := &overlayController{run: cd.run}
	owner := &SysOps{}
	d := testOverlayDefaults(owner)
	d.physicalV4 = Nexthop{}

	promoteAll(t, c, owner, d)

	assert.False(t, cd.has(overlayServiceKey), "without a scoped physical default NetBird's own sockets would follow the overlay")
}

func TestOverlayControllerRequiresOverlayIPv6(t *testing.T) {
	cd := newIPv4OnlyConfigd()
	c := &overlayController{run: cd.run}
	owner := &SysOps{}
	d := testOverlayDefaults(owner)
	d.addr.IPv6, d.addr.IPv6Net = netip.Addr{}, netip.Prefix{}

	promoteAll(t, c, owner, d)

	assert.False(t, cd.has(overlayServiceKey), "an overlay without IPv6 cannot make configd request AAAA records")
}

func TestOverlayControllerRollsBackRefusedPublish(t *testing.T) {
	// scutil exits 0 when configd refuses a key, so only the read back tells.
	cd := newIPv4OnlyConfigd()
	cd.refuse = map[string]bool{overlayServiceDNSKey: true}
	c := &overlayController{run: cd.run}
	owner := &SysOps{}
	d := testOverlayDefaults(owner)

	require.NoError(t, c.setDefault(d, netip.MustParsePrefix("0.0.0.0/0"), true))
	require.NoError(t, c.setResolver(owner, netip.MustParseAddrPort("100.91.255.254:53")))
	err := c.setDefault(d, netip.MustParsePrefix("::/0"), true)

	require.Error(t, err, "a refused entity should fail the promotion")
	assert.Contains(t, err.Error(), overlayServiceDNSKey)
	for _, key := range []string{overlayServiceKey, overlayServiceIPv6Key, overlayServiceIPv4Key} {
		assert.False(t, cd.has(key), "%s should be rolled back", key)
	}
	assert.False(t, c.snapshot().Active, "state should not report a promotion that was rolled back")
}

func TestOverlayControllerRepublishesOnResolverChange(t *testing.T) {
	cd := newIPv4OnlyConfigd()
	c := &overlayController{run: cd.run}
	owner := &SysOps{}
	promoteAll(t, c, owner, testOverlayDefaults(owner))

	require.NoError(t, c.setResolver(owner, netip.MustParseAddrPort("100.91.255.254:5053")))

	dns := cd.store[overlayServiceDNSKey]
	assert.Equal(t, "5053", dns.values["ServerPort"], "the new resolver port should be published")
	assert.Equal(t, []string{"lan", "corp.example"}, dns.arrays["SearchDomains"], "the search list captured at promotion is kept")
	assert.True(t, cd.has(overlayServiceIPv4Key), "the overlay stays primary")
}

func TestOverlayControllerRemoveAll(t *testing.T) {
	cd := newIPv4OnlyConfigd()
	// Keys a crashed daemon left behind.
	cd.store[overlayServiceKey] = fakeDict{values: map[string]string{"PrimaryRank": "First"}}
	cd.store[overlayServiceIPv4Key] = fakeDict{values: map[string]string{"Router": "100.91.146.163"}}
	c := &overlayController{run: cd.run}

	require.NoError(t, c.removeAll())

	assert.False(t, cd.has(overlayServiceKey))
	assert.False(t, cd.has(overlayServiceIPv4Key))
}

func TestOverlayControllerScutilFailure(t *testing.T) {
	cd := newIPv4OnlyConfigd()
	c := &overlayController{run: func(string) (string, error) { return "", errors.New("scutil: exit status 1") }}
	owner := &SysOps{}
	d := testOverlayDefaults(owner)

	require.NoError(t, c.setDefault(d, netip.MustParsePrefix("0.0.0.0/0"), true))
	require.NoError(t, c.setResolver(owner, netip.MustParseAddrPort("100.91.255.254:53")))
	err := c.setDefault(d, netip.MustParsePrefix("::/0"), true)

	assert.Error(t, err, "a failing scutil should be reported to the caller, which only logs it")
	assert.False(t, cd.has(overlayServiceKey))
	assert.False(t, c.snapshot().Active)
}

func promoteAll(t *testing.T, c *overlayController, resolverOwner any, d overlayDefaults) {
	t.Helper()
	require.NoError(t, c.setDefault(d, netip.MustParsePrefix("0.0.0.0/0"), true))
	require.NoError(t, c.setDefault(d, netip.MustParsePrefix("::/0"), true))
	require.NoError(t, c.setResolver(resolverOwner, netip.MustParseAddrPort("100.91.255.254:53")))
}

func testOverlayDefaults(owner *SysOps) overlayDefaults {
	return overlayDefaults{
		owner: owner,
		intf:  &net.Interface{Index: 23, Name: "utun100"},
		addr: wgaddr.Address{
			IP:      netip.MustParseAddr("100.91.146.163"),
			Network: netip.MustParsePrefix("100.91.0.0/16"),
			IPv6:    netip.MustParseAddr("fd84:622:a6ea:4a41:969b:f793:4860:d14a"),
			IPv6Net: netip.MustParsePrefix("fd84:622:a6ea:4a41::/64"),
		},
		physicalV4: Nexthop{
			IP:   netip.MustParseAddr("10.251.254.5"),
			Intf: &net.Interface{Index: 12, Name: "en0"},
		},
	}
}

// newIPv4OnlyConfigd returns a configd whose physical primary has IPv4 and DNS
// but no IPv6.
func newIPv4OnlyConfigd() *fakeConfigd {
	return &fakeConfigd{store: map[string]fakeDict{
		globalIPv4Key: {values: map[string]string{
			"PrimaryInterface": "en0",
			"PrimaryService":   "PHYS-SERVICE",
			"Router":           "10.251.254.5",
		}},
		globalDNSKey: {
			values: map[string]string{"DomainName": "lan"},
			arrays: map[string][]string{
				"SearchDomains":   {"lan", "corp.example"},
				"ServerAddresses": {"10.251.254.5"},
			},
		},
	}}
}

// fakeConfigd runs the scutil commands the overlay controller uses against an
// in-memory dynamic store, printing show output the way scutil does.
type fakeConfigd struct {
	store map[string]fakeDict
	// refuse lists keys whose set silently fails, as scutil reports a failed
	// command on stdout and still exits 0.
	refuse map[string]bool
}

type fakeDict struct {
	values map[string]string
	arrays map[string][]string
}

func (f *fakeConfigd) has(key string) bool {
	_, ok := f.store[key]
	return ok
}

func (f *fakeConfigd) run(commands string) (string, error) {
	var (
		out strings.Builder
		cur fakeDict
	)
	for _, line := range strings.Split(strings.TrimSpace(commands), "\n") {
		cmd, arg, _ := strings.Cut(line, " ")
		switch cmd {
		case "open", "quit":
		case "d.init":
			cur = fakeDict{values: map[string]string{}, arrays: map[string][]string{}}
		case "d.add":
			addToFakeDict(cur, arg)
		case "set":
			if f.refuse[arg] {
				out.WriteString("  Permission denied\n")
				continue
			}
			f.store[arg] = cur
		case "remove":
			delete(f.store, arg)
		case "show":
			f.show(&out, arg)
		default:
			return "", fmt.Errorf("unexpected scutil command %q", line)
		}
	}
	return out.String(), nil
}

func addToFakeDict(d fakeDict, arg string) {
	fields := strings.Fields(arg)
	key, rest := fields[0], fields[1:]
	if len(rest) > 0 && rest[0] == "*" {
		rest = rest[1:]
		if len(rest) > 0 && rest[0] == "#" {
			rest = rest[1:]
		}
		d.arrays[key] = rest
		return
	}
	if len(rest) > 0 && rest[0] == "#" {
		rest = rest[1:]
	}
	d.values[key] = strings.Join(rest, " ")
}

func (f *fakeConfigd) show(out *strings.Builder, key string) {
	d, ok := f.store[key]
	if !ok {
		out.WriteString("  No such key\n")
		return
	}

	keys := make([]string, 0, len(d.values)+len(d.arrays))
	for k := range d.values {
		keys = append(keys, k)
	}
	for k := range d.arrays {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out.WriteString("<dictionary> {\n")
	for _, k := range keys {
		if values, ok := d.arrays[k]; ok {
			fmt.Fprintf(out, "  %s : <array> {\n", k)
			for i, v := range values {
				fmt.Fprintf(out, "    %d : %s\n", i, v)
			}
			out.WriteString("  }\n")
			continue
		}
		fmt.Fprintf(out, "  %s : %s\n", k, d.values[k])
	}
	out.WriteString("}\n")
}

func TestEarlyBindTarget(t *testing.T) {
	en0 := &net.Interface{Index: 12, Name: "en0"}
	utun := &net.Interface{Index: 23, Name: "utun100"}

	assert.Equal(t, en0, earlyBindTarget(Nexthop{IP: netip.MustParseAddr("10.251.254.5"), Intf: en0}, nil, "utun100"),
		"the physical default interface is bound")
	assert.Nil(t, earlyBindTarget(Nexthop{Intf: utun}, nil, "utun100"),
		"a default through the overlay itself would loop NetBird's sockets into the tunnel")
	assert.Nil(t, earlyBindTarget(Nexthop{}, errors.New("route not found"), "utun100"),
		"without a default there is nothing to bind to")
	assert.Nil(t, earlyBindTarget(Nexthop{IP: netip.MustParseAddr("10.0.0.1")}, nil, "utun100"),
		"a nexthop without an interface cannot be bound to")
}
