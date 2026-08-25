//go:build !linux

// dial.go
package dispatcher

import "net"

// dialFromLB opens a connection to remoteAddress with its source address
// bound to the load balancer's interface, which is what actually steers the
// traffic out of the chosen link.
//
// The index is only used for log context by the Linux implementation.
func dialFromLB(lb *LoadBalancer, _ int, remoteAddress string) (net.Conn, error) {
	// The network is pinned to the link's family: a source bound to one
	// family cannot reach destinations of the other.
	network := networkFor(lb.IsIPv6)

	localTCPAddr, err := net.ResolveTCPAddr(network, lb.Address)
	if err != nil {
		return nil, err
	}
	remoteTCPAddr, err := net.ResolveTCPAddr(network, remoteAddress)
	if err != nil {
		return nil, err
	}
	return net.DialTCP(network, localTCPAddr, remoteTCPAddr)
}
