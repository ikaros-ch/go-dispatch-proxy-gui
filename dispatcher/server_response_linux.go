//go:build linux

// server_response_linux.go
package dispatcher

import (
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"syscall"
)

// serverResponse implements the SOCKS5 server response for Linux systems.
func (d *Dispatcher) serverResponse(localConn net.Conn, remoteAddress string) {
	lb, i := d.getLoadBalancer()
	localTCPAddr, _ := net.ResolveTCPAddr("tcp4", lb.Address)

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

	remoteConn, err := dialer.Dial("tcp4", remoteAddress)
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
