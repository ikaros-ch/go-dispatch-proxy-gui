import './style.css';
import './app.css';

import {ListInterfaces, TestConnections, StartProxy, StopProxy, GetStatus} from '../wailsjs/go/main/App';
import {main, dispatcher} from '../wailsjs/go/models';
import {EventsOn} from '../wailsjs/runtime/runtime';

type Mode = 'normal' | 'tunnel';

interface Row {
    id: string;
    address: string;      // interface IP (normal mode) or host:port (tunnel mode)
    label: string;        // interface name, or blank in tunnel mode
    enabled: boolean;
    ratio: number;
    latencyMs?: number;
    throughputBps?: number;
    testError?: string;
}

let mode: Mode = 'normal';
let rows: Row[] = [];
let lhost = '127.0.0.1';
let lport = 8080;
let quiet = false;
let running = false;
let listenAddr = '';
let testing = false;
let starting = false;
let startError = '';
let liveStats: dispatcher.LoadBalancer[] = [];

const app = document.querySelector<HTMLDivElement>('#app')!;

function fmtBytes(n: number): string {
    if (!n) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    let v = n;
    while (v >= 1024 && i < units.length - 1) {
        v /= 1024;
        i++;
    }
    return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

function fmtBps(bytesPerSec: number | undefined): string {
    if (!bytesPerSec) return '—';
    return `${fmtBytes(bytesPerSec)}/s`;
}

function newRowId(): string {
    return Math.random().toString(36).slice(2, 9);
}

async function loadInterfaces() {
    const ifaces = await ListInterfaces();
    const existing = new Map(rows.map(r => [r.address, r]));
    rows = ifaces.map((iface: dispatcher.InterfaceInfo) => {
        const prev = existing.get(iface.IP);
        return {
            id: prev?.id ?? newRowId(),
            address: iface.IP,
            label: iface.Name,
            enabled: prev?.enabled ?? true,
            ratio: prev?.ratio ?? 1,
            latencyMs: prev?.latencyMs,
            throughputBps: prev?.throughputBps,
            testError: prev?.testError,
        };
    });
    render();
}

function addTunnelRow() {
    rows.push({id: newRowId(), address: '', label: '', enabled: true, ratio: 1});
    render();
}

function removeRow(id: string) {
    rows = rows.filter(r => r.id !== id);
    render();
}

function setMode(m: Mode) {
    if (mode === m) return;
    mode = m;
    rows = [];
    if (mode === 'normal') {
        loadInterfaces();
    } else {
        addTunnelRow();
        render();
    }
}

async function runTests() {
    const targets = rows.filter(r => r.enabled && r.address);
    if (targets.length === 0) return;
    testing = true;
    render();
    try {
        const summary: main.TestSummary = await TestConnections(targets.map(r => r.address));
        const byIP = new Map(summary.results.map(r => [r.IP, r]));
        for (const row of rows) {
            const result = byIP.get(row.address);
            if (!result) continue;
            row.latencyMs = result.LatencyMs;
            row.throughputBps = result.ThroughputBps;
            row.testError = result.Error || undefined;
        }
        (window as any).__suggestedRatios = summary.suggestedRatios;
    } finally {
        testing = false;
        render();
    }
}

function applySuggested() {
    const suggested: Record<string, number> = (window as any).__suggestedRatios ?? {};
    for (const row of rows) {
        if (suggested[row.address] != null) {
            row.ratio = suggested[row.address];
        }
    }
    render();
}

async function start() {
    startError = '';
    const balancers: main.LBConfig[] = rows
        .filter(r => r.enabled && r.address)
        .map(r => ({address: r.address, contentionRatio: r.ratio} as main.LBConfig));

    if (balancers.length === 0) {
        startError = mode === 'normal'
            ? 'Select at least one interface.'
            : 'Add at least one tunnel endpoint (host:port).';
        render();
        return;
    }

    starting = true;
    render();
    try {
        const config: main.ProxyConfig = {
            lhost,
            lport,
            tunnel: mode === 'tunnel',
            quiet,
            balancers,
        } as main.ProxyConfig;
        await StartProxy(config);
        await refreshStatus();
    } catch (err: any) {
        startError = String(err?.message ?? err);
    } finally {
        starting = false;
        render();
    }
}

async function stop() {
    starting = true;
    render();
    try {
        await StopProxy();
    } finally {
        starting = false;
        await refreshStatus();
    }
}

async function refreshStatus() {
    const status: main.Status = await GetStatus();
    running = status.running;
    listenAddr = status.listenAddr;
    liveStats = status.loadBalancers ?? [];
    render();
}

const logLines: string[] = [];

function appendLog(line: string) {
    logLines.push(line);
    if (logLines.length > 400) logLines.splice(0, logLines.length - 400);
    const el = document.querySelector<HTMLPreElement>('#log');
    if (el) {
        el.textContent = logLines.join('\n');
        el.scrollTop = el.scrollHeight;
    }
}

function render() {
    app.innerHTML = `
        <h1>Go Dispatch Proxy</h1>

        <div class="panel">
            <h2>Mode</h2>
            <div class="mode-toggle">
                <button data-action="mode-normal" class="${mode === 'normal' ? 'active' : ''}">Combine interfaces</button>
                <button data-action="mode-tunnel" class="${mode === 'tunnel' ? 'active' : ''}">Load-balance tunnels</button>
            </div>
            <p class="hint">${mode === 'normal'
        ? 'Combine several of this machine\'s own network interfaces (e.g. Wi-Fi + Ethernet) into one SOCKS5 proxy.'
        : 'Load-balance across already-running SSH -D tunnels or remote SOCKS endpoints (host:port).'}</p>
        </div>

        <div class="panel">
            <h2>Load balancers</h2>
            ${mode === 'normal' ? `
                <div class="row">
                    <button data-action="refresh-ifaces">Refresh interfaces</button>
                    <button data-action="test" ${testing ? 'disabled' : ''}>${testing ? 'Testing…' : 'Test connections'}</button>
                    <button data-action="apply-suggested">Use suggested ratios</button>
                </div>
            ` : `
                <div class="row">
                    <button data-action="add-row">Add tunnel endpoint</button>
                    <button data-action="test" ${testing ? 'disabled' : ''}>${testing ? 'Testing…' : 'Test connections'}</button>
                    <button data-action="apply-suggested">Use suggested ratios</button>
                </div>
            `}
            <table>
                <thead>
                    <tr>
                        <th></th>
                        <th>${mode === 'normal' ? 'Interface' : 'Endpoint (host:port)'}</th>
                        ${mode === 'normal' ? '<th>IP</th>' : ''}
                        <th>Latency</th>
                        <th>Throughput</th>
                        <th>Ratio</th>
                        ${mode === 'tunnel' ? '<th></th>' : ''}
                    </tr>
                </thead>
                <tbody>
                    ${rows.map(rowHtml).join('')}
                </tbody>
            </table>
            ${rows.length === 0 ? '<p class="hint">No load balancers yet.</p>' : ''}
        </div>

        <div class="panel">
            <h2>Server</h2>
            <div class="row">
                <label>Listen host</label>
                <input type="text" id="lhost" value="${lhost}" ${running ? 'disabled' : ''}/>
                <label>Listen port</label>
                <input type="number" id="lport" value="${lport}" ${running ? 'disabled' : ''}/>
                <label><input type="checkbox" id="quiet" ${quiet ? 'checked' : ''} ${running ? 'disabled' : ''}/> Quiet (disable logs)</label>
            </div>
        </div>

        <div class="panel">
            <h2>Control</h2>
            <div class="row">
                <span><span class="status-dot ${running ? 'on' : ''}"></span>${running ? `Running on ${listenAddr}` : 'Stopped'}</span>
                <div class="spacer"></div>
                <button data-action="start" ${running || starting ? 'disabled' : ''}>${starting && !running ? 'Starting…' : 'Start'}</button>
                <button class="danger" data-action="stop" ${!running || starting ? 'disabled' : ''}>${starting && running ? 'Stopping…' : 'Stop'}</button>
            </div>
            ${startError ? `<p class="error-text">${escapeHtml(startError)}</p>` : ''}
        </div>

        ${running && liveStats.length > 0 ? `
        <div class="panel">
            <h2>Live stats</h2>
            <table>
                <thead>
                    <tr><th>Address</th><th>Ratio</th><th>Connections</th><th>Sent</th><th>Received</th><th>Last error</th></tr>
                </thead>
                <tbody>
                    ${liveStats.map(lb => `
                        <tr>
                            <td class="mono">${escapeHtml(lb.Address)}</td>
                            <td class="mono">${lb.ContentionRatio}</td>
                            <td class="mono">${lb.ConnectionsHandled}</td>
                            <td class="mono">${fmtBytes(lb.BytesSent)}</td>
                            <td class="mono">${fmtBytes(lb.BytesReceived)}</td>
                            <td class="error-text">${escapeHtml(lb.LastError ?? '')}</td>
                        </tr>
                    `).join('')}
                </tbody>
            </table>
        </div>` : ''}

        <div class="panel">
            <h2>Log</h2>
            <pre id="log">${escapeHtml(logLines.join('\n'))}</pre>
        </div>
    `;

    wireEvents();

    const logEl = document.querySelector<HTMLPreElement>('#log');
    if (logEl) logEl.scrollTop = logEl.scrollHeight;
}

function rowHtml(row: Row): string {
    const speed = row.testError
        ? `<span class="error-text" title="${escapeHtml(row.testError)}">failed</span>`
        : (row.throughputBps != null ? fmtBps(row.throughputBps) : '—');
    const latency = row.testError ? '—' : (row.latencyMs != null ? `${row.latencyMs.toFixed(0)} ms` : '—');

    if (mode === 'normal') {
        return `
            <tr>
                <td><input type="checkbox" data-row="${row.id}" data-field="enabled" ${row.enabled ? 'checked' : ''} ${running ? 'disabled' : ''}/></td>
                <td>${escapeHtml(row.label)}</td>
                <td class="mono">${escapeHtml(row.address)}</td>
                <td class="mono">${latency}</td>
                <td class="mono">${speed}</td>
                <td><input type="number" min="1" max="10" data-row="${row.id}" data-field="ratio" value="${row.ratio}" ${running ? 'disabled' : ''}/></td>
            </tr>
        `;
    }

    return `
        <tr>
            <td><input type="checkbox" data-row="${row.id}" data-field="enabled" ${row.enabled ? 'checked' : ''} ${running ? 'disabled' : ''}/></td>
            <td><input type="text" placeholder="127.0.0.1:7777" data-row="${row.id}" data-field="address" value="${escapeHtml(row.address)}" ${running ? 'disabled' : ''}/></td>
            <td class="mono">${latency}</td>
            <td class="mono">${speed}</td>
            <td><input type="number" min="1" max="10" data-row="${row.id}" data-field="ratio" value="${row.ratio}" ${running ? 'disabled' : ''}/></td>
            <td><button class="link" data-action="remove-row" data-row="${row.id}" ${running ? 'disabled' : ''}>Remove</button></td>
        </tr>
    `;
}

function escapeHtml(s: string): string {
    return s.replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]!));
}

function wireEvents() {
    app.querySelectorAll<HTMLElement>('[data-action]').forEach(el => {
        el.addEventListener('click', () => {
            const action = el.dataset.action;
            const rowId = el.dataset.row;
            if (action === 'mode-normal') setMode('normal');
            else if (action === 'mode-tunnel') setMode('tunnel');
            else if (action === 'refresh-ifaces') loadInterfaces();
            else if (action === 'add-row') addTunnelRow();
            else if (action === 'remove-row' && rowId) removeRow(rowId);
            else if (action === 'test') runTests();
            else if (action === 'apply-suggested') applySuggested();
            else if (action === 'start') start();
            else if (action === 'stop') stop();
        });
    });

    app.querySelectorAll<HTMLInputElement>('input[data-row]').forEach(el => {
        el.addEventListener('change', () => {
            const rowId = el.dataset.row!;
            const field = el.dataset.field!;
            const row = rows.find(r => r.id === rowId);
            if (!row) return;
            if (field === 'enabled') row.enabled = el.checked;
            else if (field === 'ratio') row.ratio = Math.max(1, Math.min(10, Number(el.value) || 1));
            else if (field === 'address') row.address = el.value.trim();
        });
    });

    const lhostEl = document.querySelector<HTMLInputElement>('#lhost');
    lhostEl?.addEventListener('change', () => lhost = lhostEl.value.trim());
    const lportEl = document.querySelector<HTMLInputElement>('#lport');
    lportEl?.addEventListener('change', () => lport = Number(lportEl.value) || 8080);
    const quietEl = document.querySelector<HTMLInputElement>('#quiet');
    quietEl?.addEventListener('change', () => quiet = quietEl.checked);
}

EventsOn('log', (line: string) => appendLog(line));
EventsOn('stats', (stats: dispatcher.LoadBalancer[]) => {
    liveStats = stats;
    if (running) render();
});

loadInterfaces();
refreshStatus();
