// dispatcher.go
package dispatcher

import (
	"context"
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
	"time"
)

// LoadBalancer is one upstream address (interface or tunnel endpoint) that
// connections can be dispatched to, along with its live stats.
type LoadBalancer struct {
	Address            string
	Iface              string
	ContentionRatio    int
	CurrentConnections int
	// IsIPv6 records the address family of this link. A source address of
	// one family cannot reach destinations of the other, so this decides
	// which connections may be dispatched here.
	IsIPv6 bool

	BytesSent          uint64
	BytesReceived      uint64
	ConnectionsHandled uint64
	LastError          string

	// Excluded links are skipped when dispatching. A link is excluded
	// automatically after repeated failures, or manually by the user, and
	// restored once it answers a health probe again.
	Excluded            bool
	ExcludedReason      string
	ConsecutiveFailures int

	// reported stops a link that keeps failing from raising the same alert
	// on every subsequent connection; it is cleared by the next success.
	reported bool
}

// Dispatcher owns the set of load balancers and the running SOCKS5/tunnel
// listener. It replaces the package-level globals used by the original CLI
// so that a GUI (or tests) can create, run, and stop multiple independent
// instances in the same process.
type Dispatcher struct {
	mu      sync.Mutex
	lbList  []LoadBalancer
	lbIndex int
	tunnel  bool
	// listener serves SOCKS5 (or tunnel) clients; httpListener serves the
	// HTTP proxy protocol that OS-level proxy settings speak. Either may be
	// absent, and both dispatch over the same load balancers.
	listener     net.Listener
	httpListener net.Listener
	running      bool

	// Health tracking. autoExclude gates whether repeated failures take a
	// link out of rotation; the health checker restores it when it works
	// again.
	autoExclude         bool
	failureThreshold    int
	recoveryInterval    time.Duration
	probeTargetOverride string
	onHealthChange      func(HealthEvent)
	healthCancel        context.CancelFunc
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
			// SplitHostPort rather than a plain split on ":", so bracketed
			// IPv6 endpoints such as [2001:db8::1]:1080 parse correctly.
			host, portStr, splitErr := net.SplitHostPort(splitted[0])
			if splitErr != nil {
				return nil, fmt.Errorf("invalid address specification %s", splitted[0])
			}

			lbIPOrFQDN = host
			lbPort, err = strconv.Atoi(portStr)
			if err != nil || lbPort <= 0 || lbPort > 65535 {
				return nil, fmt.Errorf("invalid port %s", splitted[0])
			}
		} else {
			lbIPOrFQDN = splitted[0]
			lbPort = 0
		}

		// FQDN not supported for non-tunnel mode: the address has to name a
		// local interface. Either family is accepted.
		if !tunnel && net.ParseIP(lbIPOrFQDN) == nil {
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

		// JoinHostPort brackets IPv6 literals, which a plain "%s:%d" would
		// produce unparseable output for.
		address := net.JoinHostPort(lbIPOrFQDN, strconv.Itoa(lbPort))

		lbList[idx] = LoadBalancer{
			Address:         address,
			Iface:           iface,
			ContentionRatio: contRatio,
			IsIPv6:          isIPv6Address(address),
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

// advanceLocked moves the round-robin cursor on. Callers must hold d.mu.
func (d *Dispatcher) advanceLocked() {
	d.lbIndex++
	if d.lbIndex >= len(d.lbList) {
		d.lbIndex = 0
	}
}

// getLoadBalancerFor picks the next load balancer whose address family the
// destination actually offers, honouring contention ratios and skipping
// links that cannot reach it.
//
// It returns errNoCompatibleLoadBalancer when, for example, the destination
// is IPv6-only and every configured link is IPv4.
func (d *Dispatcher) getLoadBalancerFor(hasV4, hasV6 bool) (*LoadBalancer, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	familyMatch := false

	for scanned := 0; scanned < len(d.lbList); scanned++ {
		index := d.lbIndex
		lb := &d.lbList[index]

		if !lb.usableFor(hasV4, hasV6) {
			d.advanceLocked()
			continue
		}
		familyMatch = true

		// Excluded links have been failing; skip them while they recover.
		if lb.Excluded {
			d.advanceLocked()
			continue
		}

		lb.CurrentConnections++
		if lb.CurrentConnections >= lb.ContentionRatio {
			lb.CurrentConnections = 0
			d.advanceLocked()
		}
		return lb, index, nil
	}

	if familyMatch {
		// Something could have carried this, but everything is excluded.
		// Falling back to a failing link beats refusing outright: the
		// exclusion may be stale and the request would fail anyway.
		for scanned := 0; scanned < len(d.lbList); scanned++ {
			index := d.lbIndex
			lb := &d.lbList[index]
			d.advanceLocked()

			if lb.usableFor(hasV4, hasV6) {
				return lb, index, nil
			}
		}
		return nil, 0, errAllLoadBalancersExcluded
	}

	return nil, 0, errNoCompatibleLoadBalancer
}

// selectLoadBalancer resolves the destination far enough to know which
// families it supports, then picks a link that can reach it.
func (d *Dispatcher) selectLoadBalancer(destination string) (*LoadBalancer, int, error) {
	host, _, err := splitHostPort(destination)
	if err != nil {
		return nil, 0, err
	}

	hasV4, hasV6, err := destinationFamilies(context.Background(), host)
	if err != nil {
		return nil, 0, err
	}

	return d.getLoadBalancerFor(hasV4, hasV6)
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
		d.setLastError(lb, err.Error())
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
	if net.ParseIP(lhost) == nil {
		return fmt.Errorf("invalid host %s", lhost)
	}
	if lport < 1 || lport > 65535 {
		return fmt.Errorf("invalid port %d", lport)
	}
	if len(d.lbList) == 0 {
		return errors.New("no load balancers configured")
	}

	// JoinHostPort brackets an IPv6 bind address, and "tcp" accepts clients
	// of either family so the proxy can be reached on ::1 as well.
	localBindAddress := net.JoinHostPort(lhost, strconv.Itoa(lport))
	l, err := net.Listen("tcp", localBindAddress)
	if err != nil {
		return fmt.Errorf("could not start local server on %s: %w", localBindAddress, err)
	}

	d.mu.Lock()
	d.listener = l
	d.running = true
	d.mu.Unlock()

	log.Println("[INFO] Local server started on", localBindAddress)

	d.startHealthChecks()

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

// StartHTTP begins listening for HTTP proxy clients on lhost:lport, in
// addition to whatever Start established. This is the protocol operating
// system proxy settings use, so it is what makes the dispatcher usable
// system wide.
//
// It may be called before or after Start; both listeners share the same load
// balancers and contention ratios.
func (d *Dispatcher) StartHTTP(lhost string, lport int) error {
	if net.ParseIP(lhost) == nil {
		return fmt.Errorf("invalid host %s", lhost)
	}
	if lport < 1 || lport > 65535 {
		return fmt.Errorf("invalid port %d", lport)
	}
	if len(d.lbList) == 0 {
		return errors.New("no load balancers configured")
	}

	d.mu.Lock()
	alreadyListening := d.httpListener != nil
	d.mu.Unlock()
	if alreadyListening {
		return errors.New("HTTP proxy is already listening")
	}

	// JoinHostPort brackets an IPv6 bind address, and "tcp" accepts clients
	// of either family so the proxy can be reached on ::1 as well.
	localBindAddress := net.JoinHostPort(lhost, strconv.Itoa(lport))
	l, err := net.Listen("tcp", localBindAddress)
	if err != nil {
		return fmt.Errorf("could not start HTTP proxy on %s: %w", localBindAddress, err)
	}

	d.mu.Lock()
	d.httpListener = l
	d.running = true
	d.mu.Unlock()

	log.Println("[INFO] HTTP proxy started on", localBindAddress)

	d.startHealthChecks()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go d.handleHTTPConnection(conn)
		}
	}()

	return nil
}

// HTTPAddr reports the address the HTTP proxy is listening on, or "" if it
// is not running.
func (d *Dispatcher) HTTPAddr() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.httpListener == nil {
		return ""
	}
	return d.httpListener.Addr().String()
}

// Stop closes both listeners, ending the accept loops.
func (d *Dispatcher) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.running {
		return nil
	}
	d.running = false

	if d.healthCancel != nil {
		d.healthCancel()
		d.healthCancel = nil
	}

	socksListener := d.listener
	d.listener = nil
	httpListener := d.httpListener
	d.httpListener = nil

	// Close both even if the first fails, so neither is left accepting.
	var firstErr error
	if socksListener != nil {
		if err := socksListener.Close(); err != nil {
			firstErr = err
		}
	}
	if httpListener != nil {
		if err := httpListener.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
			next[i].Excluded = old.Excluded
			next[i].ExcludedReason = old.ExcludedReason
			next[i].ConsecutiveFailures = old.ConsecutiveFailures
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
//
// The byte and connection counters are incremented atomically by in-flight
// connections, so they are read the same way rather than copied wholesale.
func (d *Dispatcher) Stats() []LoadBalancer {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]LoadBalancer, len(d.lbList))
	for i := range d.lbList {
		lb := &d.lbList[i]
		out[i] = LoadBalancer{
			Address:             lb.Address,
			Iface:               lb.Iface,
			ContentionRatio:     lb.ContentionRatio,
			CurrentConnections:  lb.CurrentConnections,
			BytesSent:           atomic.LoadUint64(&lb.BytesSent),
			BytesReceived:       atomic.LoadUint64(&lb.BytesReceived),
			ConnectionsHandled:  atomic.LoadUint64(&lb.ConnectionsHandled),
			LastError:           lb.LastError,
			Excluded:            lb.Excluded,
			ExcludedReason:      lb.ExcludedReason,
			ConsecutiveFailures: lb.ConsecutiveFailures,
		}
	}
	return out
}

// setLastError records a dispatch failure against lb. Connection handlers
// run concurrently with Stats, so this is guarded rather than assigned
// directly.
func (d *Dispatcher) setLastError(lb *LoadBalancer, message string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	lb.LastError = message
}

// isDestinationError reports whether err describes a problem with the
// requested destination rather than with the link we dispatched over.
//
// A name that does not resolve, or that resolves only to addresses this
// IPv4-only dispatcher cannot use, says nothing about the health of the
// interface. Windows constantly probes hosts like ipv6.msftconnecttest.com
// and the deliberately unresolvable disabled.invalid, so attributing those
// to a load balancer would show a permanent error against a healthy link.
func isDestinationError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var addrErr *net.AddrError
	return errors.As(err, &addrErr)
}

// recordDialFailure logs a failed dial and, when the link itself is at
// fault, records it against the load balancer so the UI can surface it.
func (d *Dispatcher) recordDialFailure(lb *LoadBalancer, i int, destination string, err error) {
	if isDestinationError(err) {
		// Left off the load balancer deliberately: the destination is at
		// fault, not this link.
		log.Println("[DEBUG] could not resolve", destination, fmt.Sprintf("{%s}", err))
		return
	}

	d.setLastError(lb, err.Error())
	log.Println("[WARN]", destination, "->", lb.Address, fmt.Sprintf("{%s}", err), "LB:", i)

	// Repeated failures in a row suggest the link itself, not the
	// destination; any success resets the streak.
	d.noteFailure(lb, err.Error())
}

// recordDialSuccess clears a link's failure streak.
func (d *Dispatcher) recordDialSuccess(lb *LoadBalancer) {
	d.noteSuccess(lb)
}
