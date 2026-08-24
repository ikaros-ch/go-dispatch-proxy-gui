package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"go-dispatch-proxy/dispatcher"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the backend bound to the frontend by Wails.
type App struct {
	ctx context.Context

	mu   sync.Mutex
	disp *dispatcher.Dispatcher

	lhost string
	lport int
}

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved so we can
// call the runtime methods, and package-level logging is redirected so log
// lines reach the frontend as "log" events.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	log.SetFlags(log.Flags() &^ (log.Ldate | log.Ltime))
	log.SetOutput(logWriter(ctx))
}

// eventLogWriter forwards everything written to it (i.e. every log.Println
// call made by the dispatcher package) to the frontend as a "log" event.
type eventLogWriter struct{ ctx context.Context }

func (w *eventLogWriter) Write(p []byte) (int, error) {
	wailsruntime.EventsEmit(w.ctx, "log", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// tolerantMultiWriter writes to every writer even if an earlier one fails,
// unlike io.MultiWriter which stops at the first error. A production build is
// compiled for the GUI subsystem and has no valid stdout, so writing to it
// errors -- with io.MultiWriter that error would suppress the frontend log
// events entirely.
type tolerantMultiWriter struct{ writers []io.Writer }

func (w *tolerantMultiWriter) Write(p []byte) (int, error) {
	for _, dst := range w.writers {
		_, _ = dst.Write(p)
	}
	return len(p), nil
}

// logWriter builds the log destination: the frontend event stream, plus
// stdout as a best-effort convenience when a console is attached.
func logWriter(ctx context.Context) io.Writer {
	return &tolerantMultiWriter{writers: []io.Writer{os.Stdout, &eventLogWriter{ctx: ctx}}}
}

// InterfaceInfo mirrors dispatcher.InterfaceInfo for the frontend.
type InterfaceInfo = dispatcher.InterfaceInfo

// ListInterfaces returns the usable local network interfaces that can be
// selected as load balancers in normal (non-tunnel) mode.
func (a *App) ListInterfaces() []InterfaceInfo {
	return dispatcher.ListInterfaces()
}

// TestSummary is the result of testing a set of interfaces, plus the
// automatically suggested contention ratio for each.
type TestSummary struct {
	Results         []dispatcher.TestResult `json:"results"`
	SuggestedRatios map[string]int          `json:"suggestedRatios"`
}

// TestConnections measures latency and throughput for each given interface
// IP (bound to that interface's source address) and suggests a contention
// ratio for each, proportional to its measured throughput.
func (a *App) TestConnections(ips []string) TestSummary {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()

	results := dispatcher.TestAllInterfaces(ctx, ips, "", 0)
	for i, r := range results {
		if name := ifaceNameForIP(r.IP); name != "" {
			results[i].InterfaceName = name
		}
	}

	return TestSummary{
		Results:         results,
		SuggestedRatios: dispatcher.SuggestRatios(results),
	}
}

func ifaceNameForIP(ip string) string {
	for _, iface := range dispatcher.ListInterfaces() {
		if iface.IP == ip {
			return iface.Name
		}
	}
	return ""
}

// LBConfig is one load balancer entry as configured from the GUI: an
// interface IP in normal mode, or a "host:port" tunnel endpoint in tunnel
// mode, plus its contention ratio.
type LBConfig struct {
	Address         string `json:"address"`
	ContentionRatio int    `json:"contentionRatio"`
}

// ProxyConfig is the full configuration submitted from the GUI's Start action.
type ProxyConfig struct {
	LHost     string     `json:"lhost"`
	LPort     int        `json:"lport"`
	Tunnel    bool       `json:"tunnel"`
	Quiet     bool       `json:"quiet"`
	Balancers []LBConfig `json:"balancers"`
}

// StartProxy validates the configuration, builds a dispatcher and starts it
// listening. Only one proxy instance runs at a time; call StopProxy first
// to reconfigure.
func (a *App) StartProxy(config ProxyConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.disp != nil && a.disp.IsRunning() {
		return fmt.Errorf("proxy is already running, stop it first")
	}

	if config.Quiet {
		log.SetOutput(io.Discard)
	} else {
		log.SetOutput(logWriter(a.ctx))
	}

	args := make([]string, len(config.Balancers))
	for i, b := range config.Balancers {
		args[i] = fmt.Sprintf("%s@%d", b.Address, b.ContentionRatio)
	}

	lbList, err := dispatcher.ParseLoadBalancers(args, config.Tunnel)
	if err != nil {
		return err
	}

	d := dispatcher.New(lbList, config.Tunnel)
	if err := d.Start(config.LHost, config.LPort); err != nil {
		return err
	}

	a.disp = d
	a.lhost = config.LHost
	a.lport = config.LPort

	go a.streamStats(d)

	return nil
}

// streamStats periodically emits a "stats" event with a snapshot of every
// load balancer's live counters while the given dispatcher is running.
func (a *App) streamStats(d *dispatcher.Dispatcher) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !d.IsRunning() {
			return
		}
		wailsruntime.EventsEmit(a.ctx, "stats", d.Stats())
	}
}

// StopProxy stops the running proxy, if any.
func (a *App) StopProxy() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.disp == nil {
		return nil
	}
	err := a.disp.Stop()
	a.disp = nil
	return err
}

// Status is a snapshot of the current proxy state for the frontend.
type Status struct {
	Running       bool                    `json:"running"`
	ListenAddr    string                  `json:"listenAddr"`
	LoadBalancers []dispatcher.LoadBalancer `json:"loadBalancers"`
}

// GetStatus reports whether the proxy is running and its current stats.
func (a *App) GetStatus() Status {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.disp == nil || !a.disp.IsRunning() {
		return Status{Running: false}
	}
	return Status{
		Running:       true,
		ListenAddr:    fmt.Sprintf("%s:%d", a.lhost, a.lport),
		LoadBalancers: a.disp.Stats(),
	}
}
