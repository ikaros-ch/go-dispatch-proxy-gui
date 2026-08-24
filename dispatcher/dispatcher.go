// dispatcher.go
package dispatcher

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// LoadBalancer is one upstream address (interface or tunnel endpoint) that
// connections can be dispatched to, along with its live stats.
type LoadBalancer struct {
	Address            string
	Iface              string
	ContentionRatio    int
	CurrentConnections int

	BytesSent          uint64
	BytesReceived      uint64
	ConnectionsHandled uint64
	LastError          string
}

// Dispatcher owns the set of load balancers and the running SOCKS5/tunnel
// listener. It replaces the package-level globals used by the original CLI
// so that a GUI (or tests) can create, run, and stop multiple independent
// instances in the same process.
type Dispatcher struct {
	mu       sync.Mutex
	lbList   []LoadBalancer
	lbIndex  int
	tunnel   bool
	listener net.Listener
	running  bool
}

// New creates a Dispatcher for the given load balancers. tunnel selects
// tunnel (transparent) mode vs normal SOCKS5 dispatch mode.
func New(lbs []LoadBalancer, tunnel bool) *Dispatcher {
	return &Dispatcher{lbList: lbs, tunnel: tunnel}
}

// ParseLoadBalancers parses the "-lb-list" style command line arguments
// (e.g. "192.168.1.2@3") into a slice of LoadBalancer. This mirrors the
// original CLI parsing rules exactly, but returns an error instead of
// calling log.Fatal so it is safe to call from a long-running GUI process.
func ParseLoadBalancers(args []string, tunnel bool) ([]LoadBalancer, error) {
	if len(args) == 0 {
		return nil, errors.New("please specify one or more load balancers")
	}

	lbList := make([]LoadBalancer, len(args))

	for idx, a := range args {
		splitted := strings.Split(a, "@")
		iface := ""
		var lbIPOrFQDN string
		var lbPort int
		var err error

		if tunnel {
			ipOrFqdnPort := strings.Split(splitted[0], ":")
			if len(ipOrFqdnPort) != 2 {
				return nil, fmt.Errorf("invalid address specification %s", splitted[0])
			}

			lbIPOrFQDN = ipOrFqdnPort[0]
			lbPort, err = strconv.Atoi(ipOrFqdnPort[1])
			if err != nil || lbPort <= 0 || lbPort > 65535 {
				return nil, fmt.Errorf("invalid port %s", splitted[0])
			}
		} else {
			lbIPOrFQDN = splitted[0]
			lbPort = 0
		}

		// FQDN not supported for non-tunnel mode
		if !tunnel && net.ParseIP(lbIPOrFQDN).To4() == nil {
			return nil, fmt.Errorf("invalid address %s", lbIPOrFQDN)
		}

		contRatio := 1
		if len(splitted) > 1 {
			contRatio, err = strconv.Atoi(splitted[1])
			if err != nil || contRatio <= 0 {
				return nil, fmt.Errorf("invalid contention ratio for %s", lbIPOrFQDN)
			}
		}

		// Obtaining the interface name of the load balancer IP doesn't make sense in tunnel mode
		if !tunnel {
			iface = GetIfaceFromIP(lbIPOrFQDN)
			if iface == "" {
				return nil, fmt.Errorf("IP address not associated with an interface %s", lbIPOrFQDN)
			}
		}

		lbList[idx] = LoadBalancer{
			Address:         fmt.Sprintf("%s:%d", lbIPOrFQDN, lbPort),
			Iface:           iface,
			ContentionRatio: contRatio,
		}
	}

	return lbList, nil
}

// getLoadBalancer picks a load balancer according to contention ratio,
// optionally skipping ones already marked as failed in _bitset (used while
// retrying a connection across the remaining load balancers).
func (d *Dispatcher) getLoadBalancer(params ...interface{}) (*LoadBalancer, int) {
	var bitset *big.Int
	if len(params) > 0 {
		seed := -1
		for _, p := range params {
			switch v := p.(type) {
			case int:
				seed = v
			case *big.Int:
				bitset = v
			}
		}
		if seed < 0 || seed >= len(d.lbList) || bitset == nil {
			bitset = nil
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if bitset != nil {
		for {
			if bitset.Bit(d.lbIndex) != 0 {
				lb := &d.lbList[d.lbIndex]
				lb.CurrentConnections = 0
				d.lbIndex++
				if d.lbIndex == len(d.lbList) {
					d.lbIndex = 0
				}
			} else {
				break
			}
		}
	}

	lb := &d.lbList[d.lbIndex]
	lb.CurrentConnections++
	ilb := d.lbIndex

	if lb.CurrentConnections == lb.ContentionRatio {
		lb.CurrentConnections = 0
		d.lbIndex++
		if d.lbIndex == len(d.lbList) {
			d.lbIndex = 0
		}
	}

	return lb, ilb
}

// pipeConnections joins the local and remote connections together,
// tallying bytes transferred against lb.
func pipeConnections(localConn, remoteConn net.Conn, lb *LoadBalancer) {
	go func() {
		defer remoteConn.Close()
		defer localConn.Close()
		n, err := io.Copy(remoteConn, localConn)
		atomic.AddUint64(&lb.BytesSent, uint64(n))
		if err != nil {
			return
		}
	}()

	go func() {
		defer remoteConn.Close()
		defer localConn.Close()
		n, err := io.Copy(localConn, remoteConn)
		atomic.AddUint64(&lb.BytesReceived, uint64(n))
		if err != nil {
			return
		}
	}()
}

// handleTunnelConnection handles a connection in tunnel mode.
func (d *Dispatcher) handleTunnelConnection(conn net.Conn) {
	lb, i := d.getLoadBalancer()
	var bitset *big.Int
	complete := 1 == len(d.lbList)

retry:
	remoteAddr, _ := net.ResolveTCPAddr("tcp4", lb.Address)
	remoteConn, err := net.DialTCP("tcp4", nil, remoteAddr)

	if err != nil {
		lb.LastError = err.Error()
		log.Println("[WARN]", lb.Address, fmt.Sprintf("{%s}", err), "LB:", i)

		if !complete && bitset == nil {
			bits := make([]byte, (len(d.lbList)+7)/8)
			bitset = new(big.Int).SetBytes(bits)
		}

		if !complete {
			bitset.SetBit(bitset, i, 1)
			mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(len(d.lbList))), big.NewInt(1))
			complete = new(big.Int).And(bitset, mask).Cmp(mask) == 0
		}

		if !complete {
			lb, i = d.getLoadBalancer(i, bitset)
			goto retry
		}

		log.Println("[WARN]", "all load balancers failed")
		conn.Close()
		return
	}

	atomic.AddUint64(&lb.ConnectionsHandled, 1)
	log.Println("[DEBUG] Tunnelled to", lb.Address, "LB:", i)
	pipeConnections(conn, remoteConn, lb)
}

// handleConnection dispatches to the tunnel or SOCKS5 handler depending on mode.
func (d *Dispatcher) handleConnection(conn net.Conn) {
	if d.tunnel {
		d.handleTunnelConnection(conn)
	} else if address, err := handleSocksConnection(conn); err == nil {
		d.serverResponse(conn, address)
	}
}

// Start begins listening for connections on lhost:lport and dispatching
// them across the configured load balancers. It returns once the listener
// is up; connections are accepted in a background goroutine.
func (d *Dispatcher) Start(lhost string, lport int) error {
	if net.ParseIP(lhost).To4() == nil {
		return fmt.Errorf("invalid host %s", lhost)
	}
	if lport < 1 || lport > 65535 {
		return fmt.Errorf("invalid port %d", lport)
	}
	if len(d.lbList) == 0 {
		return errors.New("no load balancers configured")
	}

	localBindAddress := fmt.Sprintf("%s:%d", lhost, lport)
	l, err := net.Listen("tcp4", localBindAddress)
	if err != nil {
		return fmt.Errorf("could not start local server on %s: %w", localBindAddress, err)
	}

	d.mu.Lock()
	d.listener = l
	d.running = true
	d.mu.Unlock()

	log.Println("[INFO] Local server started on", localBindAddress)

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go d.handleConnection(conn)
		}
	}()

	return nil
}

// Stop closes the listener, ending the accept loop started by Start.
func (d *Dispatcher) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.running {
		return nil
	}
	d.running = false
	l := d.listener
	d.listener = nil
	return l.Close()
}

// IsRunning reports whether the dispatcher is currently listening.
func (d *Dispatcher) IsRunning() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

// UpdateLoadBalancers swaps the active set of load balancers without
// interrupting the listener, so connections keep being served while the
// configuration changes underneath. Accumulated counters are carried over
// for addresses that survive the change; new addresses start at zero.
//
// Connections already dispatched keep using their original load balancer:
// they hold a pointer into the previous slice, which stays alive until they
// finish.
func (d *Dispatcher) UpdateLoadBalancers(lbs []LoadBalancer) error {
	if len(lbs) == 0 {
		return errors.New("no load balancers configured")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	previous := make(map[string]*LoadBalancer, len(d.lbList))
	for i := range d.lbList {
		previous[d.lbList[i].Address] = &d.lbList[i]
	}

	next := make([]LoadBalancer, len(lbs))
	copy(next, lbs)
	for i := range next {
		if old, ok := previous[next[i].Address]; ok {
			next[i].BytesSent = atomic.LoadUint64(&old.BytesSent)
			next[i].BytesReceived = atomic.LoadUint64(&old.BytesReceived)
			next[i].ConnectionsHandled = atomic.LoadUint64(&old.ConnectionsHandled)
			next[i].LastError = old.LastError
		}
	}

	d.lbList = next
	d.lbIndex = 0
	return nil
}

// Addresses returns the address of every configured load balancer.
func (d *Dispatcher) Addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.lbList))
	for i := range d.lbList {
		out[i] = d.lbList[i].Address
	}
	return out
}

// Stats returns a snapshot of every load balancer's current stats.
func (d *Dispatcher) Stats() []LoadBalancer {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]LoadBalancer, len(d.lbList))
	copy(out, d.lbList)
	return out
}
