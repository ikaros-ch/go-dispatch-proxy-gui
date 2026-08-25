// interfaces.go
package dispatcher

import "net"

// InterfaceInfo describes one usable (up, non-loopback) network interface
// address that can be used as a load balancer in normal mode.
type InterfaceInfo struct {
	Name string
	IP   string
	// IsIPv6 distinguishes the two families, which cannot reach each other:
	// an IPv6 source only serves IPv6 destinations and vice versa.
	IsIPv6 bool
}

// usableAddress reports whether an interface address can carry dispatched
// traffic.
//
// Link-local addresses are excluded: IPv6 link-local (fe80::/10) requires a
// zone index to be usable and is not routable off-link, and IPv4 link-local
// (169.254.0.0/16) means the interface failed to get a real address.
func usableAddress(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() {
		return false
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return ip.IsGlobalUnicast()
}

// ListInterfaces returns the addresses which can be used for dispatching in
// non-tunnelling mode. Alternate to ipconfig/ifconfig.
//
// Both IPv4 and IPv6 addresses are returned; an interface with both appears
// once per family, since each is dispatched over separately.
func ListInterfaces() []InterfaceInfo {
	var result []InterfaceInfo
	ifaces, _ := net.Interfaces()

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp != net.FlagUp || iface.Flags&net.FlagLoopback == net.FlagLoopback {
			continue
		}

		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || !usableAddress(ipnet.IP) {
				continue
			}
			result = append(result, InterfaceInfo{
				Name:   iface.Name,
				IP:     ipnet.IP.String(),
				IsIPv6: ipnet.IP.To4() == nil,
			})
		}
	}

	return result
}

// GetIfaceFromIP returns the interface name (NUL-terminated, matching the
// syscall.BindToDevice convention used on Linux) associated with ip, or ""
// if no up, non-loopback interface has that address.
func GetIfaceFromIP(ip string) string {
	target := net.ParseIP(ip)
	if target == nil {
		return ""
	}

	ifaces, _ := net.Interfaces()

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp != net.FlagUp || iface.Flags&net.FlagLoopback == net.FlagLoopback {
			continue
		}

		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || !usableAddress(ipnet.IP) {
				continue
			}
			if ipnet.IP.Equal(target) {
				return iface.Name + "\x00"
			}
		}
	}
	return ""
}
