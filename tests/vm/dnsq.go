//go:build ignore

// dnsq asks one DNS server for a name's IPv4 addresses and prints them, one
// per line, or "error: ..." (the router has no dig, and CI hosts may not).
//
//	go run tests/vm/dnsq.go -server 127.0.0.1:5353 ads.example.com.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"time"
)

func main() {
	server := flag.String("server", "127.0.0.1:53", "DNS server (address:port)")
	flag.Parse()
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", *server)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range flag.Args() {
		ips, err := r.LookupIP(ctx, "ip4", name)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		for _, ip := range ips {
			fmt.Println(ip)
		}
	}
}
