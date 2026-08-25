package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"go-dispatch-proxy/dispatcher"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	// autoInterfaceInterval is how often the set of usable interfaces is
	// checked. Short enough that unplugging a cable or dropping Wi-Fi is
	// noticed quickly, long enough to stay cheap: it only lists interfaces.
	autoInterfaceInterval = 10 * time.Second

	// autoRetestInterval is how often throughput is re-measured even when
	// nothing changed, so ratios track a link that has slowed down. This
	// costs real bandwidth, hence the much longer period.
	autoRetestInterval = 10 * time.Minute

	// autoTestDuration keeps each direction short; auto mode favours a
	// quick usable estimate over an accurate benchmark.
	autoTestDuration = 2 * time.Second
)

// autoManager watches for interfaces appearing or disappearing and keeps the
// running dispatcher's load balancers in step with them.
type autoManager struct {
	app *App

	cancel context.CancelFunc
	done   chan struct{}

	// busy guards against a slow test run overlapping the next tick.
	busy sync.Mutex
}

// startAutoMode begins monitoring. Calling it while already running is a
// no-op, so it is safe to call from both StartProxy and a UI toggle.
func (a *App) startAutoMode() {
	a.autoMu.Lock()
	defer a.autoMu.Unlock()

	if a.auto != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &autoManager{app: a, cancel: cancel, done: make(chan struct{})}
	a.auto = m

	go m.run(ctx)
	log.Println("[INFO] Auto mode enabled: watching interfaces every", autoInterfaceInterval)
}

// stopAutoMode ends monitoring and waits for the loop to finish, so a
// subsequent start cannot race against the previous one.
func (a *App) stopAutoMode() {
	a.autoMu.Lock()
	m := a.auto
	a.auto = nil
	a.autoMu.Unlock()

	if m == nil {
		return
	}
	m.cancel()
	<-m.done
	log.Println("[INFO] Auto mode disabled")
}

// run is the monitor loop.
func (m *autoManager) run(ctx context.Context) {
	defer close(m.done)

	interfaceTicker := time.NewTicker(autoInterfaceInterval)
	defer interfaceTicker.Stop()
	retestTicker := time.NewTicker(autoRetestInterval)
	defer retestTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-interfaceTicker.C:
			m.sync(ctx, false)
		case <-retestTicker.C:
			m.sync(ctx, true)
		}
	}
}

// sync reconciles the dispatcher's load balancers with the interfaces that
// are currently up. forceRetest re-measures throughput even when the set of
// interfaces is unchanged.
func (m *autoManager) sync(ctx context.Context, forceRetest bool) {
	// Skip this tick rather than queue up behind a test still in progress.
	if !m.busy.TryLock() {
		return
	}
	defer m.busy.Unlock()

	disp, tunnel := m.app.currentDispatcher()
	if disp == nil || !disp.IsRunning() {
		return
	}
	// Auto mode reasons about local interfaces, which have no meaning when
	// dispatching to remote tunnel endpoints.
	if tunnel {
		return
	}

	available := dispatcher.ListInterfaces()
	if len(available) == 0 {
		log.Println("[WARN] Auto mode: no usable interfaces found, keeping current configuration")
		return
	}

	desired := make([]string, 0, len(available))
	for _, iface := range available {
		desired = append(desired, iface.IP)
	}
	sort.Strings(desired)

	current := addressesToIPs(disp.Addresses())
	sort.Strings(current)

	added, removed := diffSets(current, desired)
	changed := len(added) > 0 || len(removed) > 0

	if !changed && !forceRetest {
		return
	}

	if changed {
		if len(added) > 0 {
			log.Println("[INFO] Auto mode: connection(s) appeared:", strings.Join(added, ", "))
		}
		if len(removed) > 0 {
			log.Println("[INFO] Auto mode: connection(s) lost:", strings.Join(removed, ", "))
		}
	} else {
		log.Println("[INFO] Auto mode: periodic re-test of", len(desired), "connection(s)")
	}

	m.reconfigure(ctx, disp, desired)
}

// reconfigure tests each candidate interface and applies the resulting set
// and ratios to the running dispatcher.
func (m *autoManager) reconfigure(ctx context.Context, disp *dispatcher.Dispatcher, ips []string) {
	testCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	results := dispatcher.TestAllInterfaces(testCtx, ips, "", autoTestDuration)
	if testCtx.Err() != nil {
		// Cancelled mid-test (auto mode turned off, or proxy stopped).
		return
	}

	ratios := dispatcher.SuggestRatios(results)

	// Only dispatch over interfaces that actually carried traffic; a link
	// that is up but has no route out would otherwise black-hole its share.
	usable := make([]string, 0, len(results))
	for _, r := range results {
		if r.Error == "" && r.DownloadBps > 0 {
			usable = append(usable, r.IP)
			continue
		}
		log.Println("[WARN] Auto mode: excluding", r.IP, "-", firstNonEmpty(r.Error, "no throughput measured"))
	}

	if len(usable) == 0 {
		log.Println("[WARN] Auto mode: no interface passed testing, keeping current configuration")
		return
	}

	args := make([]string, len(usable))
	for i, ip := range usable {
		ratio := ratios[ip]
		if ratio < 1 {
			ratio = 1
		}
		args[i] = fmt.Sprintf("%s@%d", ip, ratio)
	}

	lbList, err := dispatcher.ParseLoadBalancers(args, false)
	if err != nil {
		log.Println("[WARN] Auto mode: could not rebuild load balancers:", err)
		return
	}

	if err := disp.UpdateLoadBalancers(lbList); err != nil {
		log.Println("[WARN] Auto mode: could not apply new configuration:", err)
		return
	}

	log.Println("[INFO] Auto mode: now dispatching over", strings.Join(args, ", "))

	// Let the UI redraw its rows against the new configuration.
	if m.app.ctx != nil {
		wailsruntime.EventsEmit(m.app.ctx, "autoUpdate", disp.Stats())
	}
}

// addressesToIPs strips the ":port" suffix that load balancer addresses
// carry so they can be compared with interface IPs.
//
// SplitHostPort is used rather than trimming at the last colon, which would
// mangle the bracketed IPv6 form "[2001:db8::1]:0".
func addressesToIPs(addresses []string) []string {
	out := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			out = append(out, host)
			continue
		}
		out = append(out, addr)
	}
	return out
}

// diffSets reports which entries are in next but not current, and vice versa.
func diffSets(current, next []string) (added, removed []string) {
	inCurrent := make(map[string]bool, len(current))
	for _, c := range current {
		inCurrent[c] = true
	}
	inNext := make(map[string]bool, len(next))
	for _, n := range next {
		inNext[n] = true
	}

	for _, n := range next {
		if !inCurrent[n] {
			added = append(added, n)
		}
	}
	for _, c := range current {
		if !inNext[c] {
			removed = append(removed, c)
		}
	}
	return added, removed
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
