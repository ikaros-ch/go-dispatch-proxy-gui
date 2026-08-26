// health.go
package dispatcher

import (
	"context"
	"log"
	"time"
)

const (
	// defaultFailureThreshold is how many consecutive failures mark a link
	// as unhealthy. Any success resets the count, so a single unreachable
	// destination cannot condemn a link that is otherwise working.
	defaultFailureThreshold = 3

	// defaultRecoveryInterval is how often excluded links are re-probed.
	defaultRecoveryInterval = 30 * time.Second

	// healthProbeTimeout bounds a single probe.
	healthProbeTimeout = 5 * time.Second
)

// defaultProbeTargets are dialled to decide whether an excluded link has
// recovered. They are well-known resolvers, reachable without DNS (which
// might itself be broken on a failing link) and answering on 443.
var defaultProbeTargets = struct{ v4, v6 string }{
	v4: "1.1.1.1:443",
	v6: "[2606:4700:4700::1111]:443",
}

// HealthEvent describes a link changing availability, so the UI can show it
// and the user can be notified.
type HealthEvent struct {
	Address  string `json:"address"`
	Excluded bool   `json:"excluded"`
	Reason   string `json:"reason"`
}

// SetAutoExclude turns automatic exclusion of failing links on or off. When
// off, failures are still counted and reported but every link keeps its
// share of the traffic.
func (d *Dispatcher) SetAutoExclude(enabled bool) {
	d.mu.Lock()
	d.autoExclude = enabled
	d.mu.Unlock()

	if enabled {
		log.Println("[INFO] Failing connections will be excluded automatically")
		return
	}

	// Turning it off puts everything back into rotation: the user has
	// asked for all links to be used.
	d.IncludeAll()
	log.Println("[INFO] Failing connections will be kept in use")
}

// OnHealthChange registers a callback invoked whenever a link is excluded or
// restored. It is called from background goroutines.
func (d *Dispatcher) OnHealthChange(fn func(HealthEvent)) {
	d.mu.Lock()
	d.onHealthChange = fn
	d.mu.Unlock()
}

// emitHealth reports a change to the registered callback, if any.
func (d *Dispatcher) emitHealth(event HealthEvent) {
	d.mu.Lock()
	fn := d.onHealthChange
	d.mu.Unlock()

	if fn != nil {
		fn(event)
	}
}

// noteSuccess clears the failure streak after a connection succeeds, so a
// link is only excluded for failures that happen back to back.
func (d *Dispatcher) noteSuccess(lb *LoadBalancer) {
	d.mu.Lock()
	lb.ConsecutiveFailures = 0
	lb.reported = false
	d.mu.Unlock()
}

// noteFailure counts a link failure and excludes the link once it has failed
// repeatedly in a row. Returns true if the link was excluded by this call.
func (d *Dispatcher) noteFailure(lb *LoadBalancer, reason string) bool {
	d.mu.Lock()

	lb.ConsecutiveFailures++
	failures := lb.ConsecutiveFailures
	threshold := d.failureThreshold
	if threshold <= 0 {
		threshold = defaultFailureThreshold
	}

	// The threshold is reported once per failing spell, whether or not the
	// link is actually taken out of rotation: the caller may have chosen to
	// be told without excluding anything.
	reachedThreshold := failures >= threshold && !lb.reported
	if reachedThreshold {
		lb.reported = true
	}

	shouldExclude := d.autoExclude && !lb.Excluded && failures >= threshold
	if shouldExclude {
		lb.Excluded = true
		lb.ExcludedReason = reason
	}
	address := lb.Address
	excluded := lb.Excluded
	d.mu.Unlock()

	if !reachedThreshold {
		return false
	}

	if excluded {
		log.Println("[WARN] Excluding", address, "after", failures, "consecutive failures:", reason)
	} else {
		log.Println("[WARN]", address, "has failed", failures, "times in a row:", reason)
	}
	d.emitHealth(HealthEvent{Address: address, Excluded: excluded, Reason: reason})
	return shouldExclude
}

// IncludeAll puts every excluded link back into rotation, for when the user
// overrides the automatic decision.
func (d *Dispatcher) IncludeAll() {
	var restored []string

	d.mu.Lock()
	for i := range d.lbList {
		lb := &d.lbList[i]
		if lb.Excluded {
			lb.Excluded = false
			lb.ExcludedReason = ""
			lb.ConsecutiveFailures = 0
			restored = append(restored, lb.Address)
		}
	}
	d.mu.Unlock()

	for _, address := range restored {
		log.Println("[INFO] Restored", address)
		d.emitHealth(HealthEvent{Address: address, Excluded: false, Reason: "restored manually"})
	}
}

// SetExcluded excludes or restores one link by address, so the user can
// override the automatic decision for a single connection.
func (d *Dispatcher) SetExcluded(address string, excluded bool) error {
	d.mu.Lock()
	var target *LoadBalancer
	for i := range d.lbList {
		if d.lbList[i].Address == address {
			target = &d.lbList[i]
			break
		}
	}
	if target == nil {
		d.mu.Unlock()
		return errUnknownLoadBalancer
	}

	target.Excluded = excluded
	target.ConsecutiveFailures = 0
	if excluded {
		target.ExcludedReason = "excluded manually"
	} else {
		target.ExcludedReason = ""
	}
	d.mu.Unlock()

	d.emitHealth(HealthEvent{Address: address, Excluded: excluded, Reason: "changed manually"})
	return nil
}

// runHealthChecks re-probes excluded links until the dispatcher stops, so a
// connection that comes back is used again without the user intervening.
func (d *Dispatcher) runHealthChecks(ctx context.Context) {
	interval := d.recoveryInterval
	if interval <= 0 {
		interval = defaultRecoveryInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.probeExcluded(ctx)
		}
	}
}

// probeExcluded tries each excluded link and restores the ones that answer.
func (d *Dispatcher) probeExcluded(ctx context.Context) {
	type candidate struct {
		index  int
		copyLB LoadBalancer
	}

	var candidates []candidate
	d.mu.Lock()
	for i := range d.lbList {
		if d.lbList[i].Excluded {
			candidates = append(candidates, candidate{index: i, copyLB: d.lbList[i]})
		}
	}
	d.mu.Unlock()

	for _, c := range candidates {
		if ctx.Err() != nil {
			return
		}

		if err := d.probeLink(ctx, &c.copyLB, c.index); err != nil {
			continue
		}

		d.mu.Lock()
		// The set may have been replaced while probing; only restore the
		// entry if it is still the same link.
		if c.index < len(d.lbList) && d.lbList[c.index].Address == c.copyLB.Address {
			d.lbList[c.index].Excluded = false
			d.lbList[c.index].ExcludedReason = ""
			d.lbList[c.index].ConsecutiveFailures = 0
		}
		d.mu.Unlock()

		log.Println("[INFO]", c.copyLB.Address, "is responding again and has been restored")
		d.emitHealth(HealthEvent{Address: c.copyLB.Address, Excluded: false, Reason: "responding again"})
	}
}

// probeLink dials a known-good target over one link to test whether it works.
func (d *Dispatcher) probeLink(ctx context.Context, lb *LoadBalancer, index int) error {
	target := d.probeTarget(lb.IsIPv6)

	conn, err := dialFromLBContext(ctx, lb, index, target, healthProbeTimeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// probeTarget returns the address probed for the given family.
func (d *Dispatcher) probeTarget(isIPv6 bool) string {
	d.mu.Lock()
	custom := d.probeTargetOverride
	d.mu.Unlock()

	if custom != "" {
		return custom
	}
	if isIPv6 {
		return defaultProbeTargets.v6
	}
	return defaultProbeTargets.v4
}

// HealthSnapshot describes one link's availability for the UI.
type HealthSnapshot struct {
	Address             string `json:"address"`
	Excluded            bool   `json:"excluded"`
	ExcludedReason      string `json:"excludedReason"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
}

// Health reports the availability of every link.
func (d *Dispatcher) Health() []HealthSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]HealthSnapshot, len(d.lbList))
	for i := range d.lbList {
		out[i] = HealthSnapshot{
			Address:             d.lbList[i].Address,
			Excluded:            d.lbList[i].Excluded,
			ExcludedReason:      d.lbList[i].ExcludedReason,
			ConsecutiveFailures: d.lbList[i].ConsecutiveFailures,
		}
	}
	return out
}

// startHealthChecks launches the recovery prober once, whichever listener
// starts first.
func (d *Dispatcher) startHealthChecks() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.healthCancel != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.healthCancel = cancel
	go d.runHealthChecks(ctx)
}
