// server_response.go
package dispatcher

import (
	"log"
	"net"
	"sync/atomic"
)

// serverResponse completes a SOCKS5 request: it picks a load balancer, dials
// the destination from that interface, and reports the outcome back to the
// client in SOCKS5 reply format.
func (d *Dispatcher) serverResponse(localConn net.Conn, remoteAddress string) {
	lb, i := d.getLoadBalancer()

	remoteConn, err := dialFromLB(lb, i, remoteAddress)
	if err != nil {
		d.recordDialFailure(lb, i, remoteAddress, err)
		localConn.Write([]byte{5, NETWORK_UNREACHABLE, 0, 1, 0, 0, 0, 0, 0, 0})
		localConn.Close()
		return
	}

	atomic.AddUint64(&lb.ConnectionsHandled, 1)
	log.Println("[DEBUG]", remoteAddress, "->", lb.Address, "LB:", i)
	localConn.Write([]byte{5, SUCCESS, 0, 1, 0, 0, 0, 0, 0, 0})
	pipeConnections(localConn, remoteConn, lb)
}
