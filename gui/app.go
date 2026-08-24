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

	mu     sync.Mutex
	disp   *dispatcher.Dispatcher
	tunnel bool

	lhost string
	lport int

	// autoMu guards auto, which is owned by the auto mode monitor rather
	// than by the proxy lifecycle above.
	autoMu sync.Mutex
	auto   *autoManager
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

	// Restore the previous session when asked to, either by the saved
	// preference or by the -autostart flag the login entry passes.
	settings := loadSettings()
	if settings.StartProxyOnLaunch || launchedForAutostart() {
		go a.resumeSavedProxy(settings)
	}
}

// resumeSavedProxy restarts the last configuration unattended. It runs off
// the startup path so a failure here cannot stop the window from appearing.
func (a *App) resumeSavedProxy(settings Settings) {
	if len(settings.LastConfig.Balancers) == 0 {
		log.Println("[WARN] Start on launch is enabled but no previous configuration was saved")
		return
	}
	if err := a.StartProxy(settings.LastConfig); err != nil {
		log.Println("[WARN] Could not start saved configuration:", err)
		return
	}
	log.Println("[INFO] Restored the previous configuration automatically")
}

// currentDispatcher returns the running dispatcher and whether it is in
// tunnel mode, for the auto mode monitor to inspect.
func (a *App) currentDispatcher() (*dispatcher.Dispatcher, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.disp, a.tunnel
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
	AutoMode  bool       `json:"autoMode"`
	Balancers []LBConfig `json:"balancers"`
}

// StartProxy validates the configuration, builds a dispatcher and starts it
// listening. Only one proxy instance runs at a time; call StopProxy first
// to reconfigure.
func (a *App) StartProxy(config ProxyConfig) error {
	if err := a.startProxyLocked(config); err != nil {
		return err
	}

	// Auto mode is started outside the lock above: the monitor goroutine
	// acquires a.mu itself, so the two mutexes are never held at once.
	if config.AutoMode && !config.Tunnel {
		a.startAutoMode()
	}

	a.persistConfig(config)
	return nil
}

// startProxyLocked performs the part of starting that mutates App state.
func (a *App) startProxyLocked(config ProxyConfig) error {
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
	a.tunnel = config.Tunnel
	a.lhost = config.LHost
	a.lport = config.LPort

	go a.streamStats(d)

	return nil
}

// persistConfig records the configuration that is now running so it can be
// restored on the next launch. A failure to save must not stop the proxy.
func (a *App) persistConfig(config ProxyConfig) {
	settings := loadSettings()
	settings.LastConfig = config
	settings.AutoMode = config.AutoMode
	if err := saveSettings(settings); err != nil {
		log.Println("[WARN] Could not save configuration:", err)
	}
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
	// Stop the monitor before taking a.mu: it waits for the monitor
	// goroutine to finish, and that goroutine acquires a.mu itself.
	a.stopAutoMode()

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.disp == nil {
		return nil
	}
	err := a.disp.Stop()
	a.disp = nil
	return err
}

// TestProxy measures latency, download and upload through the running proxy
// itself, so the figure reflects what a real client gets across all load
// balancers combined rather than any single interface.
func (a *App) TestProxy() (dispatcher.TestResult, error) {
	a.mu.Lock()
	running := a.disp != nil && a.disp.IsRunning()
	addr := fmt.Sprintf("%s:%d", a.lhost, a.lport)
	tunnel := a.tunnel
	a.mu.Unlock()

	if !running {
		return dispatcher.TestResult{}, fmt.Errorf("the proxy is not running")
	}
	if tunnel {
		// The listener speaks SOCKS5 in both modes, so this still works;
		// the note keeps the reported IP meaningful for the UI.
		log.Println("[INFO] Testing through the proxy in tunnel mode")
	}

	log.Println("[INFO] Testing throughput through the proxy at", addr)

	ctx, cancel := context.WithTimeout(a.ctx, 90*time.Second)
	defer cancel()

	result := dispatcher.TestThroughProxy(ctx, addr, "", 0)
	result.IP = addr
	result.InterfaceName = "via proxy"

	if result.Error != "" {
		log.Println("[WARN] Proxy test failed:", result.Error)
	} else {
		log.Println("[INFO] Proxy test complete")
	}
	return result, nil
}

// SetAutoMode turns interface monitoring on or off while the proxy runs.
func (a *App) SetAutoMode(enabled bool) error {
	_, tunnel := a.currentDispatcher()
	if enabled && tunnel {
		return fmt.Errorf("auto mode applies to local interfaces, not tunnel endpoints")
	}

	if enabled {
		a.startAutoMode()
	} else {
		a.stopAutoMode()
	}

	settings := loadSettings()
	settings.AutoMode = enabled
	if err := saveSettings(settings); err != nil {
		log.Println("[WARN] Could not save auto mode preference:", err)
	}
	return nil
}

// AppSettings is the settings view handed to the frontend, including whether
// starting at login is even possible on this platform.
type AppSettings struct {
	StartAtLogin        bool `json:"startAtLogin"`
	StartAtLoginSupport bool `json:"startAtLoginSupported"`
	StartProxyOnLaunch  bool `json:"startProxyOnLaunch"`
	AutoMode            bool `json:"autoMode"`
}

// GetSettings reports the persisted preferences, reading the startup entry
// from the OS rather than trusting the saved copy, which the user may have
// changed outside the app.
func (a *App) GetSettings() AppSettings {
	settings := loadSettings()

	atLogin := settings.StartAtLogin
	if autostartSupported() {
		if actual, err := getAutostart(); err == nil {
			atLogin = actual
		}
	}

	return AppSettings{
		StartAtLogin:        atLogin,
		StartAtLoginSupport: autostartSupported(),
		StartProxyOnLaunch:  settings.StartProxyOnLaunch,
		AutoMode:            settings.AutoMode,
	}
}

// SetStartAtLogin registers or removes the OS startup entry.
func (a *App) SetStartAtLogin(enabled bool) error {
	if !autostartSupported() {
		return fmt.Errorf("starting at login is not supported on this platform")
	}
	if err := setAutostart(enabled); err != nil {
		return err
	}

	settings := loadSettings()
	settings.StartAtLogin = enabled
	if err := saveSettings(settings); err != nil {
		log.Println("[WARN] Could not save start-at-login preference:", err)
	}

	if enabled {
		log.Println("[INFO] Go Dispatch Proxy will start when you sign in")
	} else {
		log.Println("[INFO] Go Dispatch Proxy will no longer start when you sign in")
	}
	return nil
}

// SetStartProxyOnLaunch controls whether the saved configuration starts
// dispatching as soon as the app opens.
func (a *App) SetStartProxyOnLaunch(enabled bool) error {
	settings := loadSettings()
	settings.StartProxyOnLaunch = enabled
	return saveSettings(settings)
}

// Status is a snapshot of the current proxy state for the frontend.
type Status struct {
	Running       bool                      `json:"running"`
	ListenAddr    string                    `json:"listenAddr"`
	LoadBalancers []dispatcher.LoadBalancer `json:"loadBalancers"`
	AutoMode      bool                      `json:"autoMode"`
}

// GetStatus reports whether the proxy is running and its current stats.
func (a *App) GetStatus() Status {
	a.autoMu.Lock()
	autoOn := a.auto != nil
	a.autoMu.Unlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.disp == nil || !a.disp.IsRunning() {
		return Status{Running: false, AutoMode: autoOn}
	}
	return Status{
		Running:       true,
		ListenAddr:    fmt.Sprintf("%s:%d", a.lhost, a.lport),
		LoadBalancers: a.disp.Stats(),
		AutoMode:      autoOn,
	}
}
