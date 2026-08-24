// main.go
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strings"

	"go-dispatch-proxy/dispatcher"
)

func printInterfaces() {
	fmt.Println("--- Listing the available adresses for dispatching")
	for _, iface := range dispatcher.ListInterfaces() {
		fmt.Printf("[+] %s, IPv4:%s\n", iface.Name, iface.IP)
	}
}

func main() {
	var lhost = flag.String("lhost", "127.0.0.1", "The host to listen for SOCKS connection")
	var lport = flag.Int("lport", 8080, "The local port to listen for SOCKS connection")
	var detect = flag.Bool("list", false, "Shows the available addresses for dispatching (non-tunnelling mode only)")
	var tunnel = flag.Bool("tunnel", false, "Use tunnelling mode (acts as a transparent load balancing proxy)")
	var quiet = flag.Bool("quiet", false, "disable logs")

	flag.Parse()
	if *detect {
		printInterfaces()
		return
	}

	// Disable timestamp in log messages
	log.SetFlags(log.Flags() &^ (log.Ldate | log.Ltime))

	if *quiet {
		log.SetOutput(io.Discard)
	}

	lbList, err := dispatcher.ParseLoadBalancers(flag.Args(), *tunnel)
	if err != nil {
		log.Fatal("[FATAL] ", err)
	}
	for idx, lb := range lbList {
		displayAddr := lb.Address
		if !*tunnel {
			displayAddr, _, _ = strings.Cut(lb.Address, ":")
		}
		log.Printf("[INFO] Load balancer %d: %s, contention ratio: %d\n", idx+1, displayAddr, lb.ContentionRatio)
	}

	if net.ParseIP(*lhost).To4() == nil {
		log.Fatal("[FATAL] Invalid host ", *lhost)
	}
	if *lport < 1 || *lport > 65535 {
		log.Fatal("[FATAL] Invalid port ", *lport)
	}

	d := dispatcher.New(lbList, *tunnel)
	if err := d.Start(*lhost, *lport); err != nil {
		log.Fatal("[FATAL] ", err)
	}

	select {}
}
