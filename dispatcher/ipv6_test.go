package dispatcher

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestParseLoadBalancersIPv6 covers address handling for IPv6 links, where a
// plain "host:port" concatenation would produce something unparseable.
func TestParseLoadBalancersIPv6(t *testing.T) {
	// Tunnel mode accepts arbitrary endpoints, so it can be tested without
	// requiring a real local IPv6 interface.
	lbs, err := ParseLoadBalancers([]string{"[2001:db8::1]:1080@3", "10.0.0.1:1080"}, true)
	if err != nil {
		t.Fatalf("ParseLoadBalancers failed: %v", err)
	}

	if lbs[0].Address != "[2001:db8::1]:1080" {
		t.Errorf("IPv6 address is %q, want [2001:db8::1]:1080", lbs[0].Address)
	}
	if !lbs[0].IsIPv6 {
		t.Error("IPv6 load balancer was not flagged as IPv6")
	}
	if lbs[0].ContentionRatio != 3 {
		t.Errorf("contention ratio is %d, want 3", lbs[0].ContentionRatio)
	}

	if lbs[1].Address != "10.0.0.1:1080" {
		t.Errorf("IPv4 address is %q, want 10.0.0.1:1080", lbs[1].Address)
	}
	if lbs[1].IsIPv6 {
		t.Error("IPv4 load balancer was flagged as IPv6")
	}

	// The address must survive a round trip through the resolver.
	if _, err := net.ResolveTCPAddr("tcp6", lbs[0].Address); err != nil {
		t.Errorf("IPv6 load balancer address does not resolve: %v", err)
	}
}

// TestGetLoadBalancerForFamily is the core of IPv6 support: a destination
// must never be dispatched over a link of the wrong family.
func TestGetLoadBalancerForFamily(t *testing.T) {
	d := New([]LoadBalancer{
		{Address: "10.0.0.1:0", ContentionRatio: 1, IsIPv6: false},
		{Address: "[2001:db8::1]:0", ContentionRatio: 1, IsIPv6: true},
	}, false)

	// An IPv4-only destination must always land on the IPv4 link.
	for i := 0; i < 4; i++ {
		lb, _, err := d.getLoadBalancerFor(true, false)
		if err != nil {
			t.Fatalf("no load balancer for an IPv4 destination: %v", err)
		}
		if lb.IsIPv6 {
			t.Fatal("IPv4 destination was dispatched over an IPv6 link")
		}
	}

	// An IPv6-only destination must always land on the IPv6 link.
	for i := 0; i < 4; i++ {
		lb, _, err := d.getLoadBalancerFor(false, true)
		if err != nil {
			t.Fatalf("no load balancer for an IPv6 destination: %v", err)
		}
		if !lb.IsIPv6 {
			t.Fatal("IPv6 destination was dispatched over an IPv4 link")
		}
	}

	// A dual-stack destination may use either, and should use both over
	// time rather than pinning to one.
	seenV4, seenV6 := false, false
	for i := 0; i < 8; i++ {
		lb, _, err := d.getLoadBalancerFor(true, true)
		if err != nil {
			t.Fatalf("no load balancer for a dual-stack destination: %v", err)
		}
		if lb.IsIPv6 {
			seenV6 = true
		} else {
			seenV4 = true
		}
	}
	if !seenV4 || !seenV6 {
		t.Errorf("dual-stack destinations did not use both links (v4=%v v6=%v)", seenV4, seenV6)
	}
}

// TestGetLoadBalancerForUnreachableFamily checks the honest failure when no
// link can carry the destination's family.
func TestGetLoadBalancerForUnreachableFamily(t *testing.T) {
	d := New([]LoadBalancer{
		{Address: "10.0.0.1:0", ContentionRatio: 1},
		{Address: "10.0.0.2:0", ContentionRatio: 1},
	}, false)

	_, _, err := d.getLoadBalancerFor(false, true)
	if !errors.Is(err, errNoCompatibleLoadBalancer) {
		t.Errorf("got error %v, want errNoCompatibleLoadBalancer", err)
	}

	// The IPv4 links must still be usable afterwards: a rejected lookup
	// must not leave the cursor in a broken state.
	if _, _, err := d.getLoadBalancerFor(true, false); err != nil {
		t.Errorf("IPv4 dispatch broke after an unreachable-family request: %v", err)
	}
}

// TestDestinationFamilies covers literals, which must not trigger a lookup,
// and the dual-stack case.
func TestDestinationFamilies(t *testing.T) {
	cases := []struct {
		host     string
		wantV4   bool
		wantV6   bool
		wantsErr bool
	}{
		{host: "10.0.0.1", wantV4: true},
		{host: "2001:db8::1", wantV6: true},
		{host: "::1", wantV6: true},
		{host: "127.0.0.1", wantV4: true},
	}

	for _, c := range cases {
		v4, v6, err := destinationFamilies(context.Background(), c.host)
		if (err != nil) != c.wantsErr {
			t.Errorf("destinationFamilies(%q) error = %v", c.host, err)
			continue
		}
		if v4 != c.wantV4 || v6 != c.wantV6 {
			t.Errorf("destinationFamilies(%q) = (v4=%v, v6=%v), want (v4=%v, v6=%v)",
				c.host, v4, v6, c.wantV4, c.wantV6)
		}
	}

	// localhost resolves locally without external DNS and should report at
	// least one family.
	v4, v6, err := destinationFamilies(context.Background(), "localhost")
	if err != nil {
		t.Fatalf("destinationFamilies(localhost) failed: %v", err)
	}
	if !v4 && !v6 {
		t.Error("localhost reported no address families")
	}
}

// TestIsIPv6Address covers the load balancer address classification.
func TestIsIPv6Address(t *testing.T) {
	cases := map[string]bool{
		"10.0.0.1:0":      false,
		"[2001:db8::1]:0": true,
		"[::1]:8080":      true,
		"192.168.1.33:0":  false,
		"2001:db8::1":     true,
		"10.0.0.1":        false,
	}
	for address, want := range cases {
		if got := isIPv6Address(address); got != want {
			t.Errorf("isIPv6Address(%q) = %v, want %v", address, got, want)
		}
	}
}

// TestHTTPProxyOverIPv6 runs the full HTTP proxy path over an IPv6 loopback
// link to an IPv6 origin, which fails if any part still assumes IPv4.
func TestHTTPProxyOverIPv6(t *testing.T) {
	if !hasIPv6Loopback() {
		t.Skip("no IPv6 loopback available")
	}

	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello over v6"))
	}))
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("could not listen on IPv6: %v", err)
	}
	origin.Listener.Close()
	origin.Listener = listener
	origin.Start()
	defer origin.Close()

	d := New([]LoadBalancer{{Address: "[::1]:0", ContentionRatio: 1, IsIPv6: true}}, false)

	proxyListener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("could not reserve an IPv6 port: %v", err)
	}
	port := proxyListener.Addr().(*net.TCPAddr).Port
	proxyListener.Close()

	if err := d.StartHTTP("::1", port); err != nil {
		t.Fatalf("StartHTTP over IPv6 failed: %v", err)
	}
	defer d.Stop()

	client := proxyClient(t, d.HTTPAddr())
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("request through IPv6 proxy failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello over v6" {
		t.Errorf("got %q, want %q", body, "hello over v6")
	}

	if got := d.Stats()[0].ConnectionsHandled; got != 1 {
		t.Errorf("dispatcher handled %d connections, want 1", got)
	}
}

// TestSocks5IPv6DestinationAddress checks the IPv6 address type the SOCKS5
// server previously rejected outright.
func TestSocks5IPv6DestinationAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not listen: %v", err)
	}
	defer listener.Close()

	addresses := make(chan string, 1)
	failures := make(chan error, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			failures <- err
			return
		}
		defer conn.Close()

		address, err := handleSocksConnection(conn)
		if err != nil {
			failures <- err
			return
		}
		conn.Write([]byte{5, SUCCESS, 0, 1, 0, 0, 0, 0, 0, 0})
		addresses <- address
		io.Copy(io.Discard, conn)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := socks5Dial(ctx, listener.Addr().String(), "[2001:db8::1]:443")
	if err != nil {
		t.Fatalf("socks5Dial with an IPv6 destination failed: %v", err)
	}
	defer conn.Close()

	select {
	case err := <-failures:
		t.Fatalf("server rejected the IPv6 destination: %v", err)
	case address := <-addresses:
		if address != "[2001:db8::1]:443" {
			t.Errorf("server parsed destination as %q, want [2001:db8::1]:443", address)
		}
		// The parsed address must be usable for dialling.
		if _, _, err := net.SplitHostPort(address); err != nil {
			t.Errorf("parsed destination is not a valid address: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the server to parse the request")
	}
}

// TestHTTPProxyMixedFamilyRouting is the end-to-end proof of family-aware
// dispatching: with one IPv4 and one IPv6 link configured, a v4-only origin
// and a v6-only origin must each be reached over the matching link, through
// the real selection and dial path.
func TestHTTPProxyMixedFamilyRouting(t *testing.T) {
	if !hasIPv6Loopback() {
		t.Skip("no IPv6 loopback available")
	}

	originV4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("v4 origin"))
	}))
	defer originV4.Close()

	originV6 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("v6 origin"))
	}))
	v6Listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("could not listen on IPv6: %v", err)
	}
	originV6.Listener.Close()
	originV6.Listener = v6Listener
	originV6.Start()
	defer originV6.Close()

	d := New([]LoadBalancer{
		{Address: "127.0.0.1:0", ContentionRatio: 1, IsIPv6: false},
		{Address: "[::1]:0", ContentionRatio: 1, IsIPv6: true},
	}, false)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	if err := d.StartHTTP("127.0.0.1", port); err != nil {
		t.Fatalf("StartHTTP failed: %v", err)
	}
	defer d.Stop()

	client := proxyClient(t, d.HTTPAddr())

	// Each request must succeed even though only one of the two links can
	// carry it; round-robin alone would send half of them over the wrong
	// family and fail.
	for i := 0; i < 4; i++ {
		for _, c := range []struct{ url, want string }{
			{originV4.URL, "v4 origin"},
			{originV6.URL, "v6 origin"},
		} {
			resp, err := client.Get(c.url)
			if err != nil {
				t.Fatalf("request to %s failed: %v", c.url, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if string(body) != c.want {
				t.Errorf("got %q from %s, want %q", body, c.url, c.want)
			}
		}
	}

	stats := d.Stats()
	if stats[0].ConnectionsHandled == 0 {
		t.Error("the IPv4 link carried no connections")
	}
	if stats[1].ConnectionsHandled == 0 {
		t.Error("the IPv6 link carried no connections")
	}
	for _, lb := range stats {
		if lb.LastError != "" {
			t.Errorf("link %s recorded an error: %s", lb.Address, lb.LastError)
		}
	}
}

// hasIPv6Loopback reports whether this machine can use IPv6 locally.
func hasIPv6Loopback() bool {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	listener.Close()
	return true
}

// TestListInterfacesExcludesLinkLocal guards the filtering rules, since a
// link-local address cannot be dispatched over without a zone index.
func TestListInterfacesExcludesLinkLocal(t *testing.T) {
	for _, iface := range ListInterfaces() {
		ip := net.ParseIP(iface.IP)
		if ip == nil {
			t.Errorf("interface %s reported an unparseable address %q", iface.Name, iface.IP)
			continue
		}
		if ip.IsLinkLocalUnicast() {
			t.Errorf("interface %s reported link-local address %s", iface.Name, iface.IP)
		}
		if ip.IsLoopback() {
			t.Errorf("interface %s reported loopback address %s", iface.Name, iface.IP)
		}
		if wantV6 := strings.Contains(iface.IP, ":"); iface.IsIPv6 != wantV6 {
			t.Errorf("address %s has IsIPv6=%v", iface.IP, iface.IsIPv6)
		}
	}
}
