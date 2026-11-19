package health

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"time"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
	"github.com/RodyMcAmp/rollgate/compiler/config"
)

const netTimeout = 2 * time.Second

// SystemEnv is the real router below root ("/"): real commands, the running
// config from the generation store, real DNS and TCP.
func SystemEnv(root string) (Env, error) {
	c, err := RunningConfig(root)
	if err != nil {
		return Env{}, err
	}
	return Env{
		Run: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		},
		Config: c,
		DNS:    lookupA,
		Dial: func(address string) error {
			conn, err := net.DialTimeout("tcp", address, netTimeout)
			if err == nil {
				conn.Close()
			}
			return err
		},
	}, nil
}

// RunningConfig returns the config the router is running: the current
// generation's (ADR 0010), or nil before the first confirmed apply.
func RunningConfig(root string) (*config.Config, error) {
	st := apply.Store{Root: root}
	n, ok, err := st.Current()
	if err != nil || !ok {
		return nil, err
	}
	g, err := st.Load(n)
	if err != nil {
		return nil, err
	}
	c, err := config.Parse(g.Source)
	if err != nil {
		return nil, fmt.Errorf("generation %d's config: %w", n, err)
	}
	return c, nil
}

// lookupA asks server ("addr:port") for name's A records with one UDP query.
// It is deliberately minimal: the checks only need to know that a resolver
// answers, and the router has no dig.
func lookupA(server, name string) ([]netip.Addr, error) {
	conn, err := net.DialTimeout("udp", server, netTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(netTimeout)); err != nil {
		return nil, err
	}
	var id [2]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	query := append(id[:], 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0) // recursion desired, one question
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		query = append(append(query, byte(len(label))), label...)
	}
	query = append(query, 0, 0, 1, 0, 1) // root, type A, class IN
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return parseA(buf[:n], id)
}

func parseA(msg []byte, id [2]byte) ([]netip.Addr, error) {
	if len(msg) < 12 || msg[0] != id[0] || msg[1] != id[1] {
		return nil, errors.New("malformed or unrelated reply")
	}
	if rcode := msg[3] & 0x0f; rcode != 0 {
		return nil, fmt.Errorf("server answered with error code %d", rcode)
	}
	qd, an := binary.BigEndian.Uint16(msg[4:]), binary.BigEndian.Uint16(msg[6:])
	off := 12
	skipName := func() bool {
		for off < len(msg) {
			l := int(msg[off])
			switch {
			case l == 0:
				off++
				return true
			case l&0xc0 == 0xc0: // compression pointer ends the name
				off += 2
				return true
			}
			off += 1 + l
		}
		return false
	}
	for range qd {
		if !skipName() {
			return nil, errors.New("truncated question")
		}
		off += 4
	}
	var addrs []netip.Addr
	for range an {
		if !skipName() || off+10 > len(msg) {
			return nil, errors.New("truncated answer")
		}
		typ, length := binary.BigEndian.Uint16(msg[off:]), int(binary.BigEndian.Uint16(msg[off+8:]))
		off += 10
		if off+length > len(msg) {
			return nil, errors.New("truncated answer")
		}
		if typ == 1 && length == 4 {
			addrs = append(addrs, netip.AddrFrom4([4]byte(msg[off:off+4])))
		}
		off += length
	}
	return addrs, nil
}
