package clusternode

import (
	"net"
	"testing"
)

// TestDetectRoutableIPs_ConcreteBind pins the "operator chose a specific
// interface" branch — bindHost wins outright, no enumeration.
func TestDetectRoutableIPs_ConcreteBind(t *testing.T) {
	got := detectRoutableIPs("192.0.2.10")
	if len(got) != 1 || !got[0].Equal(net.ParseIP("192.0.2.10")) {
		t.Fatalf("concrete bind: want [192.0.2.10], got %v", got)
	}
}

// TestDetectRoutableIPs_LoopbackBind treats explicit loopback bind as
// "no routable IPs to advertise" — caller layers loopback on top
// elsewhere, this should NOT duplicate it from the bind.
func TestDetectRoutableIPs_LoopbackBind(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "::1"} {
		got := detectRoutableIPs(bind)
		// Either nil (interface enum failed) or an enumeration result
		// that doesn't include the loopback we asked about.
		for _, ip := range got {
			if ip.IsLoopback() {
				t.Errorf("bind=%q produced loopback %s in routable set", bind, ip)
			}
		}
	}
}

// TestDetectRoutableIPs_WildcardEnumerates pins that 0.0.0.0 / "" / ::
// trigger interface enumeration. We can't assert specific IPs (CI host
// dependent) but we CAN assert the result excludes loopback /
// unspecified / multicast / link-local-unicast.
func TestDetectRoutableIPs_WildcardEnumerates(t *testing.T) {
	for _, bind := range []string{"", "0.0.0.0", "::"} {
		got := detectRoutableIPs(bind)
		for _, ip := range got {
			switch {
			case ip.IsLoopback():
				t.Errorf("bind=%q: loopback %s leaked", bind, ip)
			case ip.IsUnspecified():
				t.Errorf("bind=%q: unspecified %s leaked", bind, ip)
			case ip.IsMulticast():
				t.Errorf("bind=%q: multicast %s leaked", bind, ip)
			case ip.IsLinkLocalUnicast():
				t.Errorf("bind=%q: link-local unicast %s leaked", bind, ip)
			}
		}
	}
}

// TestAdvertiseIPs_OperatorOverride pins that an explicitly-configured
// AdvertiseIPs slice wins outright — no enumeration, no loopback layer.
func TestAdvertiseIPs_OperatorOverride(t *testing.T) {
	override := []net.IP{net.ParseIP("203.0.113.5")}
	cfg := Config{AdvertiseIPs: override, BindHost: "0.0.0.0"}
	got := cfg.advertiseIPs()
	if len(got) != 1 || !got[0].Equal(override[0]) {
		t.Fatalf("override: want [203.0.113.5], got %v", got)
	}
}

// TestIsNoisyInterface pins the interface-name blocklist so a future
// reader who renames an entry knows the wire effect (every shipped
// cluster cert from before the rename would have included or excluded
// IPs from the affected interface). Apple-specific entries reflect
// macOS surface; VM/container entries reflect Linux + macOS overlap.
func TestIsNoisyInterface(t *testing.T) {
	noisy := []string{
		"awdl0", "awdl1",
		"llw0",
		"anpi0", "anpi1", "anpi2",
		"gif0",
		"stf0",
		"bridge0", "bridge100",
		"vmnet1", "vmnet8",
		"vboxnet0",
		"docker0",
		"br-abc123def",
		"veth1234",
		"ap0", "ap1", "ap42",
	}
	for _, n := range noisy {
		if !isNoisyInterface(n) {
			t.Errorf("expected %q to be noisy", n)
		}
	}
	// Names that look adjacent to the patterns but should NOT match —
	// utun is Tailscale/WireGuard territory and must pass through;
	// "ap-prod" is plausible operator naming and must not collide
	// with the Apple ap[N] pattern.
	clean := []string{
		"utun0", "utun1", "utun13",
		"en0", "en7",
		"eth0", "eth1",
		"wlan0",
		"lo0", "lo",
		"tailscale0",
		"wg0",
		"ap-prod-01",
		"ap",
		"apN",
	}
	for _, n := range clean {
		if isNoisyInterface(n) {
			t.Errorf("expected %q to be clean", n)
		}
	}
}

// TestDetectRoutableIPs_HostInterfaces is a sanity check on the live
// machine running the test: whatever the CI host has, the returned
// set must not contain IPs from interfaces matching the noisy
// prefixes. Pure-stdlib so it works on any platform; CI hosts
// typically don't run AirDrop, but bridge0 / docker0 / vmnet are
// realistic on dev laptops and would have leaked under the prior
// pure-InterfaceAddrs walk.
func TestDetectRoutableIPs_HostInterfaces(t *testing.T) {
	ips := detectRoutableIPs("")
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("net.Interfaces unavailable: %v", err)
	}
	noisyIPs := map[string]string{} // ip → interface
	for _, iface := range ifaces {
		if !isNoisyInterface(iface.Name) {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				noisyIPs[ipnet.IP.String()] = iface.Name
			}
		}
	}
	for _, ip := range ips {
		if iface, leaked := noisyIPs[ip.String()]; leaked {
			t.Errorf("noisy interface %q leaked IP %s into routable set", iface, ip)
		}
	}
}

// TestAdvertiseIPs_EmptyLayersLoopback pins that absent AdvertiseIPs
// always leaves loopback in the SAN so localhost dispatches keep
// working — closes the regression where a multi-network worker would
// have had its loopback-dialable surface lose a SAN entry.
func TestAdvertiseIPs_EmptyLayersLoopback(t *testing.T) {
	cfg := Config{BindHost: "192.0.2.10"}
	got := cfg.advertiseIPs()
	hasV4Loopback := false
	hasV6Loopback := false
	hasBind := false
	for _, ip := range got {
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			hasV4Loopback = true
		}
		if ip.Equal(net.IPv6loopback) {
			hasV6Loopback = true
		}
		if ip.Equal(net.ParseIP("192.0.2.10")) {
			hasBind = true
		}
	}
	if !hasV4Loopback || !hasV6Loopback {
		t.Errorf("loopback missing: %v", got)
	}
	if !hasBind {
		t.Errorf("bind IP missing: %v", got)
	}
}
