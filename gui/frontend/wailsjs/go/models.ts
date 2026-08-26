export namespace dispatcher {
	
	export class InterfaceInfo {
	    Name: string;
	    IP: string;
	    IsIPv6: boolean;
	
	    static createFrom(source: any = {}) {
	        return new InterfaceInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Name = source["Name"];
	        this.IP = source["IP"];
	        this.IsIPv6 = source["IsIPv6"];
	    }
	}
	export class LoadBalancer {
	    Address: string;
	    Iface: string;
	    ContentionRatio: number;
	    CurrentConnections: number;
	    IsIPv6: boolean;
	    BytesSent: number;
	    BytesReceived: number;
	    ConnectionsHandled: number;
	    LastError: string;
	    Excluded: boolean;
	    ExcludedReason: string;
	    ConsecutiveFailures: number;
	
	    static createFrom(source: any = {}) {
	        return new LoadBalancer(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Address = source["Address"];
	        this.Iface = source["Iface"];
	        this.ContentionRatio = source["ContentionRatio"];
	        this.CurrentConnections = source["CurrentConnections"];
	        this.IsIPv6 = source["IsIPv6"];
	        this.BytesSent = source["BytesSent"];
	        this.BytesReceived = source["BytesReceived"];
	        this.ConnectionsHandled = source["ConnectionsHandled"];
	        this.LastError = source["LastError"];
	        this.Excluded = source["Excluded"];
	        this.ExcludedReason = source["ExcludedReason"];
	        this.ConsecutiveFailures = source["ConsecutiveFailures"];
	    }
	}
	export class TestResult {
	    IP: string;
	    InterfaceName: string;
	    LatencyMs: number;
	    DownloadBps: number;
	    UploadBps: number;
	    Error: string;
	    UploadError: string;
	
	    static createFrom(source: any = {}) {
	        return new TestResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.IP = source["IP"];
	        this.InterfaceName = source["InterfaceName"];
	        this.LatencyMs = source["LatencyMs"];
	        this.DownloadBps = source["DownloadBps"];
	        this.UploadBps = source["UploadBps"];
	        this.Error = source["Error"];
	        this.UploadError = source["UploadError"];
	    }
	}

}

export namespace main {
	
	export class AppSettings {
	    startAtLogin: boolean;
	    startAtLoginSupported: boolean;
	    startProxyOnLaunch: boolean;
	    autoMode: boolean;
	    systemProxySupported: boolean;
	    failureAction: string;
	    notifyOnFailure: boolean;
	    notificationsSupported: boolean;
	
	    static createFrom(source: any = {}) {
	        return new AppSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.startAtLogin = source["startAtLogin"];
	        this.startAtLoginSupported = source["startAtLoginSupported"];
	        this.startProxyOnLaunch = source["startProxyOnLaunch"];
	        this.autoMode = source["autoMode"];
	        this.systemProxySupported = source["systemProxySupported"];
	        this.failureAction = source["failureAction"];
	        this.notifyOnFailure = source["notifyOnFailure"];
	        this.notificationsSupported = source["notificationsSupported"];
	    }
	}
	export class LBConfig {
	    address: string;
	    contentionRatio: number;
	
	    static createFrom(source: any = {}) {
	        return new LBConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.address = source["address"];
	        this.contentionRatio = source["contentionRatio"];
	    }
	}
	export class ProxyConfig {
	    lhost: string;
	    lport: number;
	    tunnel: boolean;
	    quiet: boolean;
	    autoMode: boolean;
	    httpPort: number;
	    systemProxy: boolean;
	    balancers: LBConfig[];
	
	    static createFrom(source: any = {}) {
	        return new ProxyConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.lhost = source["lhost"];
	        this.lport = source["lport"];
	        this.tunnel = source["tunnel"];
	        this.quiet = source["quiet"];
	        this.autoMode = source["autoMode"];
	        this.httpPort = source["httpPort"];
	        this.systemProxy = source["systemProxy"];
	        this.balancers = this.convertValues(source["balancers"], LBConfig);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Status {
	    running: boolean;
	    listenAddr: string;
	    loadBalancers: dispatcher.LoadBalancer[];
	    autoMode: boolean;
	    httpAddr: string;
	    systemProxy: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Status(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.listenAddr = source["listenAddr"];
	        this.loadBalancers = this.convertValues(source["loadBalancers"], dispatcher.LoadBalancer);
	        this.autoMode = source["autoMode"];
	        this.httpAddr = source["httpAddr"];
	        this.systemProxy = source["systemProxy"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TestSummary {
	    results: dispatcher.TestResult[];
	    suggestedRatios: Record<string, number>;
	
	    static createFrom(source: any = {}) {
	        return new TestSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.results = this.convertValues(source["results"], dispatcher.TestResult);
	        this.suggestedRatios = source["suggestedRatios"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

