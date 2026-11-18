//go:build ignore

// listen is a test helper for tests/netns/run.sh: it accepts TCP connections on
// a port (IPv4 and IPv6) and, for each one, appends the peer's IP address to a
// log file, then closes it. The test uses the log to check NAT (which address a
// connection arrived from).
//
// Usage: go run tests/netns/listen.go <port> <logfile>
package main

import (
	"fmt"
	"net"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: listen <port> <logfile>")
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", ":"+os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log, err := os.OpenFile(os.Args[2], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		fmt.Fprintln(log, host)
		conn.Close()
	}
}
