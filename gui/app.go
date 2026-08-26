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
	// httpPort is the HTTP proxy port, or 0 when only SOCKS5 is served.
	httpPort int
	// systemProxyApplied records that the OS settings currently point at us
	// and must be restored when the proxy stops.
	systemProxyApplied bool

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

	// A previous run that was killed before it could restore the system
	// proxy would leave this machine pointing at a port nothing is serving,
	// which looks like a total loss of internet. Repair that first.
	settings := loadSettings()
	if settings.SystemProxyActive {
		log.Println("[WARN] A previous session left the system proxy enabled; restoring the earlier settings")
		a.disableSystemProxy()
		settings = loadSettings()
	}

	// Restore the previous session when asked to, either by the saved
	// preference or by the -autostart flag the login entry passes.
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
	LHost    string `json:"lhost"`
	LPort    int    `json:"lport"`
	Tunnel   bool   `json:"tunnel"`
	Quiet    bool   `json:"quiet"`
	AutoMode bool   `json:"autoMode"`
	// HTTPPort, when non-zero, additionally serves the HTTP proxy protocol
	// that operating system proxy settings speak.
	HTTPPort int `json:"httpPort"`
	// SystemProxy points the OS at the HTTP proxy above, so every
	// application that honours system settings uses it without being
	// configured individually.
	SystemProxy bool       `json:"systemProxy"`
	Balancers   []LBConfig `json:"balancers"`
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

	// Redirecting the whole system is the last step, so it only happens
	// once there is a listener actually able to serve it.
	if config.SystemProxy && config.HTTPPort > 0 {
		if err := a.enableSystemProxy(config); err != nil {
			// The proxy itself is running fine; report the failure without
			// tearing it down.
			log.Println("[WARN] Could not apply system proxy settings:", err)
		}
	}

	return nil
}

// enableSystemProxy captures the current OS proxy settings, records them for
// crash recovery, and points the system at our HTTP listener.
func (a *App) enableSystemProxy(config ProxyConfig) error {
	if !systemProxySupported() {
		return fmt.Errorf("setting the system proxy is not supported on this platform")
	}

	previous, err := readSystemProxy()
	if err != nil {
		return err
	}

	// Persist before applying: if the machine loses power immediately
	// afterwards, the next launch still knows what to put back.
	settings := loadSettings()
	settings.SavedSystemProxy = previous
	settings.SystemProxyActive = true
	if err := saveSettings(settings); err != nil {
		return fmt.Errorf("could not record the previous proxy settings: %w", err)
	}

	address := fmt.Sprintf("%s:%d", config.LHost, config.HTTPPort)
	if err := applySystemProxy(address); err != nil {
		return err
	}

	a.mu.Lock()
	a.systemProxyApplied = true
	a.mu.Unlock()

	log.Println("[INFO] System proxy now points at", address)
	return nil
}

// disableSystemProxy restores the settings captured by enableSystemProxy.
func (a *App) disableSystemProxy() {
	a.mu.Lock()
	applied := a.systemProxyApplied
	a.systemProxyApplied = false
	a.mu.Unlock()

	settings := loadSettings()
	if !applied && !settings.SystemProxyActive {
		return
	}

	if err := restoreSystemProxy(settings.SavedSystemProxy); err != nil {
		log.Println("[WARN] Could not restore the previous system proxy settings:", err)
		// Leave the recovery flag set so the next launch tries again.
		return
	}

	settings.SystemProxyActive = false
	if err := saveSettings(settings); err != nil {
		log.Println("[WARN] Could not clear the system proxy record:", err)
	}
	log.Println("[INFO] System proxy settings restored")
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

	if config.HTTPPort > 0 {
		if err := d.StartHTTP(config.LHost, config.HTTPPort); err != nil {
			// Leave nothing half-started: the SOCKS listener is already up.
			d.Stop()
			return err
		}
	}

	// Health handling is configured before anything can fail.
	settings := loadSettings()
	action := settings.normalisedFailureAction()
	d.SetAutoExclude(action == FailureActionExclude)
	d.OnHealthChange(func(event dispatcher.HealthEvent) {
		a.handleHealthEvent(event, action, settings.NotifyOnFailure)
	})

	a.disp = d
	a.tunnel = config.Tunnel
	a.lhost = config.LHost
	a.lport = config.LPort
	a.httpPort = config.HTTPPort

	go a.streamStats(d)

	return nil
}

// handleHealthEvent reacts to a connection failing or recovering: it tells
// the UI, optionally raises a desktop notification, and applies the action
// the user chose.
func (a *App) handleHealthEvent(event dispatcher.HealthEvent, action string, notify bool) {
	if a.ctx != nil {
		wailsruntime.EventsEmit(a.ctx, "health", event)
	}

	if action == FailureActionIgnore {
		return
	}

	title := "Connection restored"
	body := fmt.Sprintf("%s is responding again and is back in use.", event.Address)
	if event.Excluded {
		title = "Connection excluded"
		body = fmt.Sprintf("%s stopped responding and is no longer being used. %s", event.Address, event.Reason)
	} else if action == FailureActionNotify && event.Reason != "" && event.Reason != "responding again" {
		// Reported without being excluded: the link is still carrying
		// traffic, which the wording has to make clear.
		title = "Connection failing"
		body = fmt.Sprintf("%s keeps failing but is still in use. %s", event.Address, event.Reason)
	}

	if notify && notificationsSupported() {
		showNotification(title, body)
	}
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

	// Hand the system back its own settings before the listener goes away,
	// so there is no window where traffic is aimed at a dead port.
	a.disableSystemProxy()

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
	// SystemProxySupported gates the UI option: only Windows can be
	// reconfigured from here.
	SystemProxySupported bool `json:"systemProxySupported"`
	// FailureAction and NotifyOnFailure control what happens when a
	// connection stops responding.
	FailureAction        string `json:"failureAction"`
	NotifyOnFailure      bool   `json:"notifyOnFailure"`
	NotificationsSupport bool   `json:"notificationsSupported"`
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
		StartAtLogin:         atLogin,
		StartAtLoginSupport:  autostartSupported(),
		StartProxyOnLaunch:   settings.StartProxyOnLaunch,
		AutoMode:             settings.AutoMode,
		SystemProxySupported: systemProxySupported(),
		FailureAction:        settings.normalisedFailureAction(),
		NotifyOnFailure:      settings.NotifyOnFailure,
		NotificationsSupport: notificationsSupported(),
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

// SetFailureHandling chooses what happens when a connection stops
// responding, and whether a desktop notification is shown. It applies to the
// running proxy immediately.
func (a *App) SetFailureHandling(action string, notify bool) error {
	settings := loadSettings()
	settings.FailureAction = action
	settings.NotifyOnFailure = notify
	normalised := settings.normalisedFailureAction()
	settings.FailureAction = normalised

	if err := saveSettings(settings); err != nil {
		return err
	}

	if disp, _ := a.currentDispatcher(); disp != nil {
		disp.SetAutoExclude(normalised == FailureActionExclude)
		disp.OnHealthChange(func(event dispatcher.HealthEvent) {
			a.handleHealthEvent(event, normalised, notify)
		})
	}
	return nil
}

// SetConnectionExcluded excludes or restores one connection by address,
// overriding the automatic decision.
func (a *App) SetConnectionExcluded(address string, excluded bool) error {
	disp, _ := a.currentDispatcher()
	if disp == nil {
		return fmt.Errorf("the proxy is not running")
	}
	return disp.SetExcluded(address, excluded)
}

// TestNotification shows a sample notification so the user can confirm they
// are working, and grant permission if Windows asks.
func (a *App) TestNotification() error {
	if !notificationsSupported() {
		return fmt.Errorf("desktop notifications are not supported on this platform")
	}
	showNotification("Go Dispatch Proxy", "Notifications are working.")
	return nil
}

// Status is a snapshot of the current proxy state for the frontend.
type Status struct {
	Running       bool                      `json:"running"`
	ListenAddr    string                    `json:"listenAddr"`
	LoadBalancers []dispatcher.LoadBalancer `json:"loadBalancers"`
	AutoMode      bool                      `json:"autoMode"`
	// HTTPAddr is the HTTP proxy address to enter in system settings, or ""
	// when only SOCKS5 is being served.
	HTTPAddr string `json:"httpAddr"`
	// SystemProxy reports whether the OS is currently pointed at us.
	SystemProxy bool `json:"systemProxy"`
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

	httpAddr := ""
	if a.httpPort > 0 {
		httpAddr = fmt.Sprintf("%s:%d", a.lhost, a.httpPort)
	}

	return Status{
		Running:       true,
		ListenAddr:    fmt.Sprintf("%s:%d", a.lhost, a.lport),
		LoadBalancers: a.disp.Stats(),
		AutoMode:      autoOn,
		HTTPAddr:      httpAddr,
		SystemProxy:   a.systemProxyApplied,
	}
}
