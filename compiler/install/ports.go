package install

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// Port is an Ethernet port.
type Port struct {
	Name    string // the installer's name for it, such as enp2s0
	MAC     string
	Driver  string // r8169, e1000e, ...
	Index   int    // the kernel's interface index: the order the ports were found in
	Carrier bool   // a cable is in, with something switched on at the other end
	// Addr is the address a DHCP server gave this port while the installer
	// asked, if one did.
	Addr netip.Prefix
}

func (p Port) String() string {
	link := "no cable"
	if p.Carrier {
		link = "cable in"
	}
	s := fmt.Sprintf("%s  %s  %s  %s", p.Name, p.MAC, p.Driver, link)
	if p.Addr.IsValid() {
		s += "  got " + p.Addr.String()
	}
	return s
}

// ReadPorts lists the wired Ethernet ports below sys (normally /sys), in the
// order the kernel found them.
func ReadPorts(sys string) ([]Port, error) {
	dir := filepath.Join(sys, "class", "net")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var ports []Port
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		// Ethernet (ARPHRD_ETHER), a real device (not a bridge or VLAN), not Wi-Fi.
		if readTrim(filepath.Join(p, "type")) != "1" || !exists(filepath.Join(p, "device")) ||
			exists(filepath.Join(p, "wireless")) || exists(filepath.Join(p, "phy80211")) {
			continue
		}
		port := Port{Name: e.Name(), MAC: readTrim(filepath.Join(p, "address"))}
		port.Index, _ = strconv.Atoi(readTrim(filepath.Join(p, "ifindex")))
		if drv, err := os.Readlink(filepath.Join(p, "device", "driver")); err == nil {
			port.Driver = filepath.Base(drv)
		}
		port.Carrier = readTrim(filepath.Join(p, "carrier")) == "1"
		ports = append(ports, port)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Index < ports[j].Index })
	return ports, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

var (
	// ErrOnePort: a router needs a port for the modem and one for home.
	ErrOnePort = errors.New("this computer has fewer than two Ethernet ports, so it can't be a router: one port goes to your modem, another to your home network")
	// ErrWhichWAN: no port, or more than one, got an address from a DHCP
	// server, so the installer has to ask which one goes to the modem.
	ErrWhichWAN = errors.New("can't tell which port goes to the modem")
)

// Choice is how the installer sets up the ports.
type Choice struct {
	WAN, LAN Port
	Extra    []Port // left unused: router.toml can't join ports into one network yet
	LANAddr  netip.Prefix
}

// ChoosePorts picks WAN as the one port that got a DHCP address, and LAN as
// the first other port with a cable in, or else the first other port.
func ChoosePorts(ports []Port) (Choice, error) {
	if len(ports) < 2 {
		return Choice{}, ErrOnePort
	}
	wan := ""
	for _, p := range ports {
		if p.Addr.IsValid() {
			if wan != "" {
				return Choice{}, ErrWhichWAN
			}
			wan = p.Name
		}
	}
	if wan == "" {
		return Choice{}, ErrWhichWAN
	}
	return ChooseWithWAN(ports, wan)
}

// ChooseWithWAN sets up the ports with wan as WAN.
func ChooseWithWAN(ports []Port, wan string) (Choice, error) {
	if len(ports) < 2 {
		return Choice{}, ErrOnePort
	}
	var c Choice
	var others []Port
	found := false
	for _, p := range ports {
		if p.Name == wan {
			c.WAN, found = p, true
		} else {
			others = append(others, p)
		}
	}
	if !found {
		return Choice{}, fmt.Errorf("no port named %s", wan)
	}
	lan := 0
	for i, p := range others {
		if p.Carrier {
			lan = i
			break
		}
	}
	c.LAN = others[lan]
	c.Extra = append(append([]Port{}, others[:lan]...), others[lan+1:]...)
	var seen []netip.Prefix
	for _, p := range ports {
		if p.Addr.IsValid() {
			seen = append(seen, p.Addr)
		}
	}
	var err error
	c.LANAddr, err = ChooseLAN(seen)
	return c, err
}

// LANChoices are the home network's addresses, in order of preference: the
// first is the one examples/router.toml uses.
var LANChoices = []netip.Prefix{
	netip.MustParsePrefix("192.168.10.1/24"),
	netip.MustParsePrefix("192.168.20.1/24"),
	netip.MustParsePrefix("10.10.0.1/24"),
	netip.MustParsePrefix("172.16.10.1/24"),
}

// ChooseLAN returns the first of LANChoices that overlaps none of the
// networks the ports saw (the modem's, above all).
func ChooseLAN(seen []netip.Prefix) (netip.Prefix, error) {
	for _, c := range LANChoices {
		clash := false
		for _, s := range seen {
			if c.Masked().Overlaps(s.Masked()) {
				clash = true
				break
			}
		}
		if !clash {
			return c, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("every home network address the installer knows (%v) overlaps your modem's network; put a router.toml on the stick instead", LANChoices)
}

// DHCPRange is the range the router hands out on LAN: .100 to .199 of its /24.
func DHCPRange(lan netip.Prefix) (start, end netip.Addr) {
	b := lan.Masked().Addr().As4()
	b[3] = 100
	start = netip.AddrFrom4(b)
	b[3] = 199
	return start, netip.AddrFrom4(b)
}
