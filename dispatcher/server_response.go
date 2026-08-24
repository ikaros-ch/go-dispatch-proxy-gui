//go:build !linux

// server_response.go
package dispatcher

import (
	"fmt"
	"log"
	"net"
	"sync/atomic"
)

// serverResponse implements the SOCKS5 server response for non-Linux systems.
func (d *Dispatcher) serverResponse(localConn net.Conn, remoteAddress string) {
	lb, i := d.getLoadBalancer()

	localTCPAddr, _ := net.ResolveTCPAddr("tcp4", lb.Address)
	remoteTCPAddr, _ := net.ResolveTCPAddr("tcp4", remoteAddress)
	remoteConn, err := net.DialTCP("tcp4", localTCPAddr, remoteTCPAddr)

	if err != nil {
		lb.LastError = err.Error()
		log.Println("[WARN]", remoteAddress, "->", lb.Address, fmt.Sprintf("{%s}", err), "LB:", i)
		localConn.Write([]byte{5, NETWORK_UNREACHABLE, 0, 1, 0, 0, 0, 0, 0, 0})
		localConn.Close()
		return
	}
	atomic.AddUint64(&lb.ConnectionsHandled, 1)
	log.Println("[DEBUG]", remoteAddress, "->", lb.Address, "LB:", i)
	localConn.Write([]byte{5, SUCCESS, 0, 1, 0, 0, 0, 0, 0, 0})
	pipeConnections(localConn, remoteConn, lb)
}
