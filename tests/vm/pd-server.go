//go:build ignore

// pd-server is a test helper for tests/vm/ipv6-test.sh: the DHCPv6 side of a
// fake ISP that delegates one IPv6 prefix (RFC 8415 prefix delegation). It
// answers Solicit with Advertise and Request, Renew and Rebind with Reply,
// always offering the same prefix, and logs each exchange to stdout.
//
// Usage: go run tests/vm/pd-server.go <interface> <prefix, e.g. 2001:db8:1000::/56>
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"syscall"
)

const (
	msgSolicit   = 1
	msgAdvertise = 2
	msgRequest   = 3
	msgRenew     = 5
	msgRebind    = 6
	msgReply     = 7

	optClientID = 1
	optServerID = 2
	optIAPD     = 25
	optIAPrefix = 26
)

// serverID is a DUID-LL (type 3, hardware type 1) with a made-up MAC.
var serverID = []byte{0, 3, 0, 1, 0x02, 0, 0, 0, 0, 0x01}

func main() {
	if len(os.Args) != 3 {
		fail(fmt.Errorf("usage: pd-server <interface> <prefix>"))
	}
	ifi, err := net.InterfaceByName(os.Args[1])
	if err != nil {
		fail(err)
	}
	prefix, err := netip.ParsePrefix(os.Args[2])
	if err != nil || !prefix.Addr().Is6() {
		fail(fmt.Errorf("bad prefix %q", os.Args[2]))
	}

	conn, err := net.ListenMulticastUDP("udp6", ifi, &net.UDPAddr{IP: net.ParseIP("ff02::1:2"), Port: 547})
	if err != nil {
		fail(err)
	}
	// Replies must leave through the ISP link, from its link-local address.
	raw, err := conn.SyscallConn()
	if err == nil {
		_ = raw.Control(func(fd uintptr) {
			_ = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifi.Name)
		})
	}
	fmt.Printf("pd-server: delegating %s on %s\n", prefix, ifi.Name)

	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			fail(err)
		}
		reply, what := answer(buf[:n], prefix)
		if reply == nil {
			continue
		}
		if _, err := conn.WriteToUDP(reply, from); err != nil {
			fmt.Println("pd-server: send:", err)
			continue
		}
		fmt.Printf("pd-server: %s to %s\n", what, from)
	}
}

// answer builds the reply to one client message, or nil to ignore it.
func answer(msg []byte, prefix netip.Prefix) ([]byte, string) {
	if len(msg) < 4 {
		return nil, ""
	}
	var replyType byte
	switch msg[0] {
	case msgSolicit:
		replyType = msgAdvertise
	case msgRequest, msgRenew, msgRebind:
		replyType = msgReply
	default:
		return nil, ""
	}
	opts := parse(msg[4:])
	clientID, iapd := opts[optClientID], opts[optIAPD]
	if clientID == nil || len(iapd) < 4 {
		return nil, "" // no client ID or no prefix requested
	}
	iaid := iapd[:4]

	// IA_PD: IAID, T1, T2, then one IA Prefix option.
	iaPrefix := make([]byte, 25)
	binary.BigEndian.PutUint32(iaPrefix[0:], 3600) // preferred lifetime
	binary.BigEndian.PutUint32(iaPrefix[4:], 7200) // valid lifetime
	iaPrefix[8] = byte(prefix.Bits())
	a := prefix.Addr().As16()
	copy(iaPrefix[9:], a[:])
	ia := append(append([]byte{}, iaid...), 0, 0, 0x07, 0x08, 0, 0, 0x0b, 0x40) // T1 1800, T2 2880
	ia = append(ia, option(optIAPrefix, iaPrefix)...)

	out := append([]byte{replyType}, msg[1:4]...) // same transaction ID
	out = append(out, option(optServerID, serverID)...)
	out = append(out, option(optClientID, clientID)...)
	out = append(out, option(optIAPD, ia)...)
	names := map[byte]string{msgAdvertise: "advertise", msgReply: "reply"}
	return out, fmt.Sprintf("%s %s", names[replyType], prefix)
}

func parse(b []byte) map[uint16][]byte {
	opts := map[uint16][]byte{}
	for len(b) >= 4 {
		code, n := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+n {
			break
		}
		opts[code] = b[4 : 4+n]
		b = b[4+n:]
	}
	return opts
}

func option(code uint16, data []byte) []byte {
	o := make([]byte, 4, 4+len(data))
	binary.BigEndian.PutUint16(o, code)
	binary.BigEndian.PutUint16(o[2:], uint16(len(data)))
	return append(o, data...)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "pd-server:", err)
	os.Exit(1)
}
