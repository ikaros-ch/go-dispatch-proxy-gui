// dialer.go
package dispatcher

import (
	"context"
	"net"
	"time"
)

// defaultDialTimeout bounds how long a single outbound connection attempt
// may take. Without it a link that has gone quiet holds the request until
// the operating system gives up, which on Windows is roughly 20 seconds.
const defaultDialTimeout = 10 * time.Second

// dialFromLB opens a connection to remoteAddress over the given link, with
// the source address bound so the traffic leaves by that interface.
func dialFromLB(lb *LoadBalancer, i int, remoteAddress string) (net.Conn, error) {
	return dialFromLBContext(context.Background(), lb, i, remoteAddress, defaultDialTimeout)
}

// dialFromLBContext is dialFromLB with an explicit deadline, used by the
// health probe as well as ordinary dispatching.
func dialFromLBContext(ctx context.Context, lb *LoadBalancer, i int, remoteAddress string, timeout time.Duration) (net.Conn, error) {
	dialer, err := dialerForLB(lb, i)
	if err != nil {
		return nil, err
	}
	dialer.Timeout = timeout

	// The network is pinned to the link's family: a source bound to one
	// family cannot reach destinations of the other.
	return dialer.DialContext(ctx, networkFor(lb.IsIPv6), remoteAddress)
}
