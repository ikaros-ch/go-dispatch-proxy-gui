//go:build linux

// dial_linux.go
package dispatcher

import (
	"log"
	"net"
	"syscall"
)

// dialFromLB opens a connection to remoteAddress bound to the load
// balancer's interface. Linux additionally uses SO_BINDTODEVICE, which is
// what makes dispatching work when several interfaces share a route.
func dialFromLB(lb *LoadBalancer, i int, remoteAddress string) (net.Conn, error) {
	// The network is pinned to the link's family: a source bound to one
	// family cannot reach destinations of the other.
	network := networkFor(lb.IsIPv6)

	localTCPAddr, err := net.ResolveTCPAddr(network, lb.Address)
	if err != nil {
		return nil, err
	}

	dialer := net.Dialer{
		LocalAddr: localTCPAddr,
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				// NOTE: Run with root or use setcap to allow interface binding
				// sudo setcap cap_net_raw=eip ./go-dispatch-proxy
				if err := syscall.BindToDevice(int(fd), lb.Iface); err != nil {
					log.Println("[WARN] Couldn't bind to interface", lb.Iface, "LB:", i)
				}
			})
		},
	}

	return dialer.Dial(network, remoteAddress)
}
