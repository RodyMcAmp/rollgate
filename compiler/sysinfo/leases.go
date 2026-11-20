package sysinfo

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// LeaseFile is where dnsmasq keeps its DHCP leases (its default on Arch).
const LeaseFile = "/var/lib/misc/dnsmasq.leases"

// Lease is a device that has an address from the router's DHCP server.
type Lease struct {
	Expires  time.Time `json:"expires,omitzero"` // zero: never
	MAC      string    `json:"mac"`
	IPv4     string    `json:"ipv4"`
	Hostname string    `json:"hostname,omitempty"` // what the device calls itself, if it said
}

// ReadLeases reads dnsmasq's lease file: one lease per line, "expiry MAC
// address hostname client-id", with "*" for an unknown hostname. IPv6 lines
// and anything it can't read are skipped. No file means no leases.
func ReadLeases(path string) ([]Lease, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var leases []Lease
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || strings.Contains(fields[2], ":") {
			continue // short, or an IPv6 lease (duid line or address)
		}
		expiry, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		l := Lease{MAC: strings.ToLower(fields[1]), IPv4: fields[2]}
		if expiry > 0 {
			l.Expires = time.Unix(expiry, 0).UTC()
		}
		if fields[3] != "*" {
			l.Hostname = fields[3]
		}
		leases = append(leases, l)
	}
	return leases, sc.Err()
}
