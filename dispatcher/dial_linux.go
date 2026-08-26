//go:build linux

// dial_linux.go
package dispatcher

import (
	"log"
	"net"
	"syscall"
)

// dialerForLB builds a dialer bound to the load balancer's interface. Linux
// additionally uses SO_BINDTODEVICE, which is what makes dispatching work
// when several interfaces share a route.
func dialerForLB(lb *LoadBalancer, i int) (*net.Dialer, error) {
	localTCPAddr, err := net.ResolveTCPAddr(networkFor(lb.IsIPv6), lb.Address)
	if err != nil {
		return nil, err
	}

	return &net.Dialer{
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
	}, nil
}
