package config

import (
	"context"
	"net"
	"strings"
)

// LinuxLinks lists non-virtual NICs via net.Interfaces. It never changes
// addresses or routes.
type LinuxLinks struct{}

func (LinuxLinks) List(_ context.Context) ([]Iface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []Iface
	for _, nif := range ifs {
		if skipIface(nif.Name) {
			continue
		}
		item := Iface{Name: nif.Name, MAC: nif.HardwareAddr.String(), Method: MethodDHCP, State: IfaceDown}
		if nif.Flags&net.FlagUp != 0 {
			item.State = IfaceUp
		}
		addrs, err := nif.Addrs()
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			ip := ipnet.IP
			if v4 := ip.To4(); v4 != nil {
				item.Address = v4.String()
				ones, _ := ipnet.Mask.Size()
				item.Prefix = ones
				break
			}
			if item.Address == "" && ip.To16() != nil && !strings.Contains(ip.String(), "%") {
				item.Address = ip.String()
				ones, _ := ipnet.Mask.Size()
				item.Prefix = ones
			}
		}
		out = append(out, item)
	}
	return out, nil
}
