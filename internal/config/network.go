package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PathIfupdown is the one managed ifupdown file, relative to Generator.Root
// (production: /etc/network/interfaces.d/hoserva). Q75: Hoserva never
// writes NetworkManager or systemd-networkd configuration.
const PathIfupdown = "network/interfaces.d/hoserva"

// ConfirmWindow is the production confirm-or-revert duration (Q75). Tests
// inject a shorter window.
const ConfirmWindow = 60 * time.Second

// Backend is the host network manager Q75 detects.
type Backend string

const (
	BackendIfupdown        Backend = "ifupdown"
	BackendNetworkManager  Backend = "networkmanager"
	BackendSystemdNetworkd Backend = "systemd-networkd"
	BackendUnknown         Backend = "unknown"
)

// AddressMethod is DHCP or static, matching the ifupdown inet stanza.
type AddressMethod string

const (
	MethodDHCP   AddressMethod = "dhcp"
	MethodStatic AddressMethod = "static"
)

// IfaceState is the kernel link's operational state.
type IfaceState string

const (
	IfaceUp   IfaceState = "up"
	IfaceDown IfaceState = "down"
)

// Iface is one host NIC the network page lists.
type Iface struct {
	Name    string
	MAC     string
	Method  AddressMethod
	Address string
	Prefix  int
	Gateway string
	DNS     []string
	State   IfaceState
}

// PendingChange is an in-flight confirm-or-revert (Q75).
type PendingChange struct {
	Interface        string
	ExpiresAt        time.Time
	RemainingSeconds int
}

// NetworkStatus is the API-facing view of host networking.
type NetworkStatus struct {
	Backend        Backend
	Editable       bool
	ReadOnlyReason string
	Interfaces     []Iface
	Pending        *PendingChange
}

// NetworkChange is one addressing apply. Access scope and listen port are
// handled outside this package.
type NetworkChange struct {
	Interface string
	Method    AddressMethod
	Address   string
	Prefix    int
	Gateway   string
	DNS       []string
	DNSSet    bool
}

var ifaceNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,14}$`)

func validIfaceName(name string) bool {
	return ifaceNameRe.MatchString(name)
}

func skipIface(name string) bool {
	switch {
	case name == "lo":
		return true
	case strings.HasPrefix(name, "docker"), strings.HasPrefix(name, "veth"),
		strings.HasPrefix(name, "br-"), strings.HasPrefix(name, "virbr"),
		strings.HasPrefix(name, "tun"), strings.HasPrefix(name, "tap"),
		strings.HasPrefix(name, "dummy"), strings.HasPrefix(name, "wg"),
		strings.HasPrefix(name, "sit"), strings.HasPrefix(name, "gre"):
		return true
	default:
		return false
	}
}

func inetFamily(ip net.IP) string {
	if ip.To4() != nil {
		return "inet"
	}
	return "inet6"
}

// RenderIfupdown renders the body of the one managed interfaces.d file
// (header is applied by Generator.Write).
func RenderIfupdown(change NetworkChange) (string, error) {
	if !validIfaceName(change.Interface) {
		return "", fmt.Errorf("%w: invalid interface name %q", ErrNetworkInvalid, change.Interface)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "auto %s\n", change.Interface)
	switch change.Method {
	case MethodDHCP:
		fmt.Fprintf(&b, "iface %s inet dhcp\n", change.Interface)
	case MethodStatic:
		ip := net.ParseIP(change.Address)
		if ip == nil {
			return "", fmt.Errorf("%w: invalid address %q", ErrNetworkInvalid, change.Address)
		}
		if change.Prefix < 0 || change.Prefix > 128 {
			return "", fmt.Errorf("%w: invalid prefix %d", ErrNetworkInvalid, change.Prefix)
		}
		if ip.To4() != nil && change.Prefix > 32 {
			return "", fmt.Errorf("%w: invalid IPv4 prefix %d", ErrNetworkInvalid, change.Prefix)
		}
		fmt.Fprintf(&b, "iface %s %s static\n", change.Interface, inetFamily(ip))
		fmt.Fprintf(&b, "    address %s/%d\n", change.Address, change.Prefix)
		if change.Gateway != "" {
			gw := net.ParseIP(change.Gateway)
			if gw == nil {
				return "", fmt.Errorf("%w: invalid gateway %q", ErrNetworkInvalid, change.Gateway)
			}
			fmt.Fprintf(&b, "    gateway %s\n", change.Gateway)
		}
	default:
		return "", fmt.Errorf("%w: method must be dhcp or static", ErrNetworkInvalid)
	}
	if len(change.DNS) > 0 {
		for _, d := range change.DNS {
			if net.ParseIP(d) == nil {
				return "", fmt.Errorf("%w: invalid DNS address %q", ErrNetworkInvalid, d)
			}
		}
		fmt.Fprintf(&b, "    dns-nameservers %s\n", strings.Join(change.DNS, " "))
	}
	return b.String(), nil
}

func parseManagedStanza(body []byte, name string) (Iface, bool) {
	out := Iface{Name: name, Method: MethodDHCP}
	found := false
	auto := "auto " + name
	ifaceDHCP := "iface " + name + " inet dhcp"
	sc := strings.Split(string(body), "\n")
	in := false
	for _, raw := range sc {
		line := strings.TrimSpace(raw)
		if line == auto {
			found = true
			continue
		}
		if strings.HasPrefix(line, "iface "+name+" ") {
			found = true
			in = true
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				switch fields[3] {
				case "dhcp":
					out.Method = MethodDHCP
				case "static":
					out.Method = MethodStatic
				}
			}
			continue
		}
		if in && strings.HasPrefix(line, "iface ") {
			in = false
			continue
		}
		if !in {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "address":
			addr, pfx, ok := strings.Cut(fields[1], "/")
			if ok {
				out.Address = addr
				n, err := strconv.Atoi(pfx)
				if err == nil {
					out.Prefix = n
				}
			} else {
				out.Address = fields[1]
			}
		case "gateway":
			out.Gateway = fields[1]
		case "dns-nameservers":
			out.DNS = append([]string(nil), fields[1:]...)
		}
	}
	if strings.Contains(string(body), ifaceDHCP) || found {
		return out, found
	}
	return Iface{}, false
}
