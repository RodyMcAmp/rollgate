package sysinfo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestReadLeases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnsmasq.leases")
	content := "1795200000 52:54:00:AA:BB:01 192.168.10.123 mayas-iphone 01:52:54:00:aa:bb:01\n" +
		"0 52:54:00:aa:bb:02 192.168.10.124 * *\n" +
		"duid 00:01:00:01:2c:5f:aa:bb:52:54:00:aa:bb:03\n" +
		"1795200000 1234 fd12:3456::50 laptop 00:01:00:01\n" +
		"garbage\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLeases(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []Lease{
		{Expires: time.Unix(1795200000, 0).UTC(), MAC: "52:54:00:aa:bb:01", IPv4: "192.168.10.123", Hostname: "mayas-iphone"},
		{MAC: "52:54:00:aa:bb:02", IPv4: "192.168.10.124"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("leases:\n%+v\nwant\n%+v", got, want)
	}
	if l, err := ReadLeases(filepath.Join(t.TempDir(), "none")); l != nil || err != nil {
		t.Errorf("no file: %v %v", l, err)
	}
}
