//go:build !linux

// dial.go
package dispatcher

import "net"

// dialerForLB builds a dialer whose source address is bound to the load
// balancer's interface, which is what actually steers traffic out of the
// chosen link.
//
// The index is only used for log context by the Linux implementation.
func dialerForLB(lb *LoadBalancer, _ int) (*net.Dialer, error) {
	localTCPAddr, err := net.ResolveTCPAddr(networkFor(lb.IsIPv6), lb.Address)
	if err != nil {
		return nil, err
	}
	return &net.Dialer{LocalAddr: localTCPAddr}, nil
}
