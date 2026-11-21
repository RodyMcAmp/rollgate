package install

import (
	"fmt"
	"net/netip"
)

// The router.toml the installer writes for a box with three Realtek ports,
// the first of them to a modem on 192.168.1.0/24.
func ExampleRouterTOML() {
	ports := []Port{
		{Name: "enp1s0", MAC: "6c:f0:49:00:00:01", Driver: "r8169", Carrier: true, Addr: netip.MustParsePrefix("192.168.1.23/24")},
		{Name: "enp2s0", MAC: "6c:f0:49:00:00:02", Driver: "r8169", Carrier: true},
		{Name: "enp3s0", MAC: "6c:f0:49:00:00:03", Driver: "r8169"},
	}
	c, err := ChoosePorts(ports)
	if err != nil {
		panic(err)
	}
	data, err := RouterTOML(c, "2026-11-20")
	if err != nil {
		panic(err)
	}
	fmt.Print(string(data))
	// Output:
	// # This router's settings, written by the rollgate installer on 2026-11-20 (ADR 0021).
	// # The first boot applies them. Change them on the web UI, or edit this file and
	// # run "rollgate apply". Every option is explained in docs/config-spec.md.
	// schema_version = 1
	//
	// [system]
	// ipv6 = "auto"   # local IPv6 always; IPv6 internet too if your ISP offers it
	//
	// [management]
	// allowed_zones = ["lan"]   # SSH and the web UI only from your home network
	//
	// # The internet: the port to your modem.
	// [[interface]]
	// name = "wan"
	// match_mac = "6c:f0:49:00:00:01"   # enp1s0, r8169; got 192.168.1.23/24 from the modem
	// ipv4 = "dhcp"
	//
	// # Your home network: plug a switch, a Wi-Fi access point or a computer in here.
	// [[interface]]
	// name = "lan"
	// match_mac = "6c:f0:49:00:00:02"   # enp2s0, r8169
	// ipv4 = "192.168.10.1/24"
	//
	// # Devices on the home network get an address from the router.
	// [dhcp.lan]
	// range = ["192.168.10.100", "192.168.10.199"]
	//
	// # This computer's other ports, unused for now: router.toml can't join several
	// # ports into one network yet.
	// #   enp3s0  6c:f0:49:00:00:03  r8169
}
