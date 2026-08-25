// family.go
package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// errNoCompatibleLoadBalancer is returned when a destination is only
// reachable over an address family none of the configured load balancers
// can provide -- typically an IPv6-only host with IPv4-only links.
var errNoCompatibleLoadBalancer = errors.New("no load balancer can reach this destination's address family")

// familyLookupTimeout bounds the name resolution done to decide which
// address families a destination offers.
const familyLookupTimeout = 5 * time.Second

// destinationFamilies reports which address families a destination can be
// reached over. A source address bound to one family cannot reach the other,
// so this decides which load balancers are eligible for the connection.
//
// Literal addresses are answered without a lookup.
func destinationFamilies(ctx context.Context, host string) (hasV4, hasV6 bool, err error) {
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			return true, false, nil
		}
		return false, true, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, familyLookupTimeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return false, false, err
	}

	for _, addr := range addrs {
		if addr.IP.To4() != nil {
			hasV4 = true
			continue
		}
		hasV6 = true
	}

	if !hasV4 && !hasV6 {
		return false, false, &net.DNSError{Err: "no addresses found", Name: host, IsNotFound: true}
	}
	return hasV4, hasV6, nil
}

// splitHostPort separates a destination into host and port, tolerating the
// bracketed form used for IPv6 literals.
func splitHostPort(address string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(address)
	if err != nil {
		return "", "", fmt.Errorf("invalid destination %q: %w", address, err)
	}
	return host, port, nil
}

// isIPv6Address reports whether an "ip:port" load balancer address refers to
// an IPv6 source.
func isIPv6Address(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() == nil
}

// networkFor returns the dial network that forces the given family, so a
// connection can only be made over the interface we bound to.
func networkFor(isIPv6 bool) string {
	if isIPv6 {
		return "tcp6"
	}
	return "tcp4"
}

// usableFor reports whether this load balancer's family is among those the
// destination offers.
func (lb *LoadBalancer) usableFor(hasV4, hasV6 bool) bool {
	if lb.IsIPv6 {
		return hasV6
	}
	return hasV4
}
