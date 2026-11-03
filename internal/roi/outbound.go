package roi

import (
	"fmt"
	"net/netip"
	"strings"
)

func allowedNetwork(raw string) (netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if ip, err := netip.ParseAddr(raw); err == nil && ip.Zone() == "" {
		ip = ip.Unmap()
		return netip.PrefixFrom(ip, ip.BitLen()), nil
	}
	p, err := proxyPrefix(raw)
	if err != nil {
		return p, fmt.Errorf("invalid poll.allowed_networks entry %q", raw)
	}
	return p, nil
}
func metadataIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLinkLocalUnicast() || ip == netip.MustParseAddr("100.100.100.200") || ip == netip.MustParseAddr("168.63.129.16") || netip.MustParsePrefix("fd00:ec2::/64").Contains(ip)
}
func (c Config) outboundIPAllowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() && netip.MustParsePrefix("64:ff9b::/96").Contains(ip) {
		v := ip.As16()
		return c.outboundIPAllowed(netip.AddrFrom4([4]byte{v[12], v[13], v[14], v[15]}))
	}
	if ip.Is6() && netip.MustParsePrefix("2002::/16").Contains(ip) {
		v := ip.As16()
		return c.outboundIPAllowed(netip.AddrFrom4([4]byte{v[2], v[3], v[4], v[5]}))
	}
	if metadataIP(ip) || !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if publicIP(ip) {
		return true
	}
	for _, raw := range c.Poll.AllowedNetworks {
		if p, err := allowedNetwork(raw); err == nil && p.Contains(ip) {
			return true
		}
	}
	// Kept for old local configurations; explicit networks are preferred.
	return c.Poll.AllowPrivate
}
