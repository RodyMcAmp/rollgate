package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/RodyMcAmp/rollgate/compiler/config"
	"github.com/RodyMcAmp/rollgate/compiler/edit"
)

// The Port forwarding page: [[firewall.port_forward]] entries as a table and
// a form. A change is made on router.toml's text by compiler/edit (ADR
// 0016), checked like the Advanced editor's text, and applied through the
// same form, so it gets the same checks and commit-confirm.

const forwardTable = "firewall.port_forward"

type forwardRow struct {
	Index                       int
	Name, Protocol, WANPort, To string
	Device                      string // the fixed-address device at To's address, if any
}

// forwardForm is the add or edit form, as typed.
type forwardForm struct {
	Index      int    // the entry being edited; -1 for a new one
	OrigName   string // the entry's name when the page loaded it
	Name       string
	Protocol   string
	WANPort    string
	Device     string // a fixed-address device's address, or "" for Address
	Address    string
	DevicePort string
}

type device struct{ Name, Address string }

// loadForwards reads router.toml's port forwards and fixed-address devices.
// It doesn't validate: a file with mistakes still lists what it has.
func loadForwards(source string) ([]forwardRow, []device) {
	var c config.Config
	_, _ = toml.Decode(source, &c)
	names := map[string]string{}
	var devices []device
	for _, d := range c.DHCP {
		for _, s := range d.Static {
			if s.Hostname != "" && s.IPv4 != "" {
				devices = append(devices, device{Name: s.Hostname, Address: s.IPv4})
				names[s.IPv4] = s.Hostname
			}
		}
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	var rows []forwardRow
	for i, f := range c.Firewall.PortForward {
		host, _, _ := strings.Cut(f.To, ":")
		rows = append(rows, forwardRow{Index: i, Name: f.Name, Protocol: f.Protocol,
			WANPort: fmt.Sprint(f.WANPort), To: f.To, Device: names[host]})
	}
	return rows, devices
}

func (s *Server) forwardsPage(w http.ResponseWriter, r *http.Request) {
	sess := s.loggedIn(w, r)
	if sess == nil {
		return
	}
	p := s.newPage(r.Context(), sess, "port forwarding")
	p.Nav = "forwards"
	p.Form = forwardForm{Index: -1, Protocol: "tcp"}
	cur, ok := s.currentConfig(r.Context(), p)
	if ok {
		p.Forwards, p.Devices = loadForwards(cur)
		if i, err := strconv.Atoi(r.URL.Query().Get("edit")); err == nil && i >= 0 && i < len(p.Forwards) {
			p.Form = formFor(p.Forwards[i], p.Devices)
		}
	}
	s.render(w, http.StatusOK, "forwards.html", p)
}

// formFor fills the form from an existing entry.
func formFor(row forwardRow, devices []device) forwardForm {
	f := forwardForm{Index: row.Index, OrigName: row.Name, Name: row.Name, Protocol: row.Protocol, WANPort: row.WANPort}
	host, port, _ := strings.Cut(row.To, ":")
	f.Address, f.DevicePort = host, port
	for _, d := range devices {
		if d.Address == host {
			f.Device, f.Address = host, ""
		}
	}
	if first, _, _ := strings.Cut(row.WANPort, "-"); first == port {
		f.DevicePort = "" // the same port
	}
	return f
}

func (s *Server) currentConfig(ctx context.Context, p *page) (string, bool) {
	cur, err := s.API.Get(ctx, "/v1/config")
	if err == nil && !cur.OK {
		err = apiError(cur)
	}
	if err != nil {
		p.Error = "Couldn't read the router's router.toml: " + err.Error()
		return "", false
	}
	return cur.Config, true
}

// forwards is the form: review a new, changed or removed forward. The
// review's Apply button posts the new router.toml to /settings.
func (s *Server) forwards(w http.ResponseWriter, r *http.Request) {
	sess := s.posted(w, r)
	if sess == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	p := s.newPage(ctx, sess, "port forwarding")
	p.Nav = "forwards"
	f := forwardForm{Index: -1, OrigName: r.PostFormValue("orig_name"), Name: strings.TrimSpace(r.PostFormValue("name")),
		Protocol: r.PostFormValue("protocol"), WANPort: strings.TrimSpace(r.PostFormValue("wan_port")),
		Device: r.PostFormValue("device"), Address: strings.TrimSpace(r.PostFormValue("address")),
		DevicePort: strings.TrimSpace(r.PostFormValue("device_port"))}
	if i, err := strconv.Atoi(r.PostFormValue("index")); err == nil && i >= 0 {
		f.Index = i
	}
	p.Form = f
	cur, ok := s.currentConfig(ctx, p)
	if !ok {
		s.render(w, http.StatusOK, "forwards.html", p)
		return
	}
	p.Forwards, p.Devices = loadForwards(cur)

	var e edit.Edit
	if r.PostFormValue("action") == "remove" {
		if f.Index < 0 {
			p.Error = "Choose a forward to remove."
			s.render(w, http.StatusOK, "forwards.html", p)
			return
		}
		e = edit.Remove(forwardTable, f.Index, "name", f.OrigName)
		p.Removing = f.OrigName
		p.Form = forwardForm{Index: -1, Protocol: "tcp"}
	} else {
		fields, err := f.fields()
		if err != nil {
			p.Error = err.Error()
			s.render(w, http.StatusOK, "forwards.html", p)
			return
		}
		if f.Index >= 0 {
			e = edit.Change(forwardTable, f.Index, "name", f.OrigName, fields)
		} else {
			e = edit.Add(forwardTable, fields...)
		}
	}
	text, err := edit.Apply([]byte(cur), e)
	if err != nil {
		var refusal *edit.Refusal
		switch {
		case errors.As(err, &refusal):
			p.Error, p.Advanced = refusal.Reason, true
		case errors.Is(err, edit.ErrStale):
			p.Error = "router.toml changed on the router since this page loaded it (over SSH, or from another browser). Here it is as it is now: make your change again."
			p.Form = forwardForm{Index: -1, Protocol: "tcp"}
		default:
			p.Error = err.Error()
		}
		s.render(w, http.StatusOK, "forwards.html", p)
		return
	}
	p.Config, p.Base = string(text), digest(cur)
	s.check(ctx, p)
	p.Config64 = base64.StdEncoding.EncodeToString(text)
	p.Reviewing = true
	s.render(w, http.StatusOK, "forwards.html", p)
}

var (
	portPattern  = regexp.MustCompile(`^[0-9]{1,5}$`)
	rangePattern = regexp.MustCompile(`^([0-9]{1,5})-([0-9]{1,5})$`)
)

func validPort(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1 && n <= 65535
}

// fields checks the form and turns it into router.toml's keys. The router's
// own validation runs after this, on the whole file.
func (f forwardForm) fields() ([]edit.Field, error) {
	if f.Name == "" {
		return nil, mistake("Give the forward a name, such as \"game server\", so you know later what it's for.")
	}
	switch f.Protocol {
	case "tcp", "udp", "tcp+udp":
	default:
		return nil, mistake("Choose TCP, UDP or both.")
	}
	var wan any
	first := 0
	switch {
	case portPattern.MatchString(f.WANPort):
		n, ok := validPort(f.WANPort)
		if !ok {
			return nil, mistake("A port is a number from 1 to 65535.")
		}
		wan, first = n, n
	case rangePattern.MatchString(f.WANPort):
		m := rangePattern.FindStringSubmatch(f.WANPort)
		a, ok1 := validPort(m[1])
		b, ok2 := validPort(m[2])
		if !ok1 || !ok2 || a >= b {
			return nil, mistake("A range of ports is two numbers from 1 to 65535, the smaller first, such as 27015-27030.")
		}
		wan, first = f.WANPort, a
	default:
		return nil, mistake("Type the port from the internet as a number, such as 25565, or a range, such as 27015-27030.")
	}
	host := f.Device
	if host == "" {
		host = f.Address
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.Is4() {
		return nil, mistake("Choose a device, or type the IPv4 address of the device at home, such as 192.168.10.20.")
	}
	port := first
	if f.DevicePort != "" {
		n, ok := validPort(f.DevicePort)
		if !ok {
			return nil, mistake("The port on the device is a number from 1 to 65535.")
		}
		if _, isRange := wan.(string); isRange && n != first {
			return nil, mistake("For a range, each port goes to the same port on the device: leave the device's port empty.")
		}
		port = n
	}
	return []edit.Field{
		{Key: "name", Value: f.Name},
		{Key: "protocol", Value: f.Protocol},
		{Key: "wan_port", Value: wan},
		{Key: "to", Value: netip.AddrPortFrom(addr, uint16(port)).String()},
	}, nil
}

// submittedConfig is the router.toml a form sent: the editor's text, or a
// form page's reviewed text, sent base64-encoded so the browser can't change
// its line endings.
func submittedConfig(r *http.Request) (string, error) {
	if b := r.PostFormValue("config64"); b != "" {
		raw, err := base64.StdEncoding.DecodeString(b)
		return string(raw), err
	}
	return editorText(r), nil
}

// diffLine is one line of a unified diff, with how to color it.
type diffLine struct{ Class, Text string }

// ChangeLines is p.Changes line by line, for coloring.
func (p *page) ChangeLines() []diffLine {
	var lines []diffLine
	for _, l := range strings.Split(strings.TrimSuffix(p.Changes, "\n"), "\n") {
		class := ""
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "@@"):
			class = "meta"
		case strings.HasPrefix(l, "+"):
			class = "add"
		case strings.HasPrefix(l, "-"):
			class = "del"
		}
		lines = append(lines, diffLine{Class: class, Text: l})
	}
	return lines
}

// mistake is a sentence for the user about what to fix in the form.
type mistake string

func (m mistake) Error() string { return string(m) }
