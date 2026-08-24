// interfaces.go
package dispatcher

import "net"

// InterfaceInfo describes one usable (up, non-loopback, IPv4) network
// interface that can be used as a load balancer address in normal mode.
type InterfaceInfo struct {
	Name string
	IP   string
}

// ListInterfaces returns the addresses which can be used for dispatching in
// non-tunnelling mode. Alternate to ipconfig/ifconfig.
func ListInterfaces() []InterfaceInfo {
	var result []InterfaceInfo
	ifaces, _ := net.Interfaces()

	for _, iface := range ifaces {
		if (iface.Flags&net.FlagUp == net.FlagUp) && (iface.Flags&net.FlagLoopback != net.FlagLoopback) {
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
					if ipnet.IP.To4() != nil {
						result = append(result, InterfaceInfo{Name: iface.Name, IP: ipnet.IP.String()})
					}
				}
			}
		}
	}

	return result
}

// GetIfaceFromIP returns the interface name (NUL-terminated, matching the
// syscall.BindToDevice convention used on Linux) associated with ip, or ""
// if no up, non-loopback interface has that address.
func GetIfaceFromIP(ip string) string {
	ifaces, _ := net.Interfaces()

	for _, iface := range ifaces {
		if (iface.Flags&net.FlagUp == net.FlagUp) && (iface.Flags&net.FlagLoopback != net.FlagLoopback) {
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
					if ipnet.IP.To4() != nil {
						if ipnet.IP.String() == ip {
							return iface.Name + "\x00"
						}
					}
				}
			}
		}
	}
	return ""
}
