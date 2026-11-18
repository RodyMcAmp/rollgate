//go:build ignore

// probe is a test helper for tests/netns/run.sh, standing in for a DHCP client
// and a DNS client on a LAN device.
//
//	probe dhcp <mac>            get an IPv4 lease (discover, offer, request, ack) and
//	                            print "address=A router=R dns=D"; exit 1 on timeout
//	probe dns <server> <name>   look up name's IPv4 addresses at server, one per line
//
// Usage: go run tests/netns/probe.go <command> ...
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	var err error
	switch {
	case len(os.Args) == 3 && os.Args[1] == "dhcp":
		err = dhcp(os.Args[2])
	case len(os.Args) == 4 && os.Args[1] == "dns":
		err = dns(os.Args[2], os.Args[3])
	default:
		err = errors.New("usage: probe dhcp <mac> | probe dns <server> <name>")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}

func dns(server, name string) error {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(server, "53"))
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addrs, err := r.LookupIP(ctx, "ip4", name)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		fmt.Println(a)
	}
	return nil
}

// DHCP message types and option codes (RFC 2131, RFC 2132).
const (
	discover, offer, request, ack = 1, 2, 3, 5
	optRouter, optDNS, optRequestedIP, optMsgType, optServerID, optParams, optEnd = 3, 6, 50, 53, 54, 55, 255
)

func dhcp(macText string) error {
	mac, err := net.ParseMAC(macText)
	if err != nil {
		return err
	}
	// Go enables SO_BROADCAST on IPv4 UDP sockets, which sending to 255.255.255.255 needs.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 68})
	if err != nil {
		return err
	}
	defer conn.Close()
	server := &net.UDPAddr{IP: net.IPv4bcast, Port: 67}
	xid := make([]byte, 4)
	rand.Read(xid)

	if _, err := conn.WriteTo(message(xid, mac, []byte{optMsgType, 1, discover, optParams, 2, optRouter, optDNS}), server); err != nil {
		return err
	}
	off, err := receive(conn, xid, offer)
	if err != nil {
		return fmt.Errorf("no offer: %w", err)
	}
	req := []byte{optMsgType, 1, request, optRequestedIP, 4}
	req = append(req, off.addr.To4()...)
	req = append(req, optServerID, 4)
	req = append(req, off.options[optServerID]...)
	req = append(req, optParams, 2, optRouter, optDNS)
	if _, err := conn.WriteTo(message(xid, mac, req), server); err != nil {
		return err
	}
	a, err := receive(conn, xid, ack)
	if err != nil {
		return fmt.Errorf("no ack: %w", err)
	}
	fmt.Printf("address=%s router=%s dns=%s\n", a.addr, net.IP(a.options[optRouter]), net.IP(a.options[optDNS]))
	return nil
}

// message builds a BOOTP request asking for a broadcast reply.
func message(xid []byte, mac net.HardwareAddr, options []byte) []byte {
	m := make([]byte, 240)
	m[0], m[1], m[2] = 1, 1, 6 // BOOTREQUEST, Ethernet, 6-byte addresses
	copy(m[4:8], xid)
	binary.BigEndian.PutUint16(m[10:12], 0x8000) // broadcast flag
	copy(m[28:34], mac)
	copy(m[236:240], []byte{99, 130, 83, 99}) // magic cookie
	return append(append(m, options...), optEnd)
}

type reply struct {
	addr    net.IP
	options map[byte][]byte
}

func receive(conn *net.UDPConn, xid []byte, want byte) (reply, error) {
	// dnsmasq pings a new address for about 3 s before offering it (to check it's
	// free), so allow more than that.
	conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return reply{}, err
		}
		m := buf[:n]
		if n < 240 || m[0] != 2 || string(m[4:8]) != string(xid) {
			continue
		}
		r := reply{addr: net.IP(append([]byte(nil), m[16:20]...)), options: map[byte][]byte{}}
		for o := m[240:]; len(o) > 0; {
			if o[0] == 0 { // pad
				o = o[1:]
				continue
			}
			if o[0] == optEnd || len(o) < 2 || len(o) < 2+int(o[1]) {
				break
			}
			r.options[o[0]] = append([]byte(nil), o[2:2+int(o[1])]...)
			o = o[2+int(o[1]):]
		}
		if t := r.options[optMsgType]; len(t) == 1 && t[0] == want {
			return r, nil
		}
	}
}
