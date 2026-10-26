// Package hostdev lists the host's USB and PCI devices, resolves passthrough ids to them,
// and claims/releases them for a QEMU VM through a small root helper.
package hostdev

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Roots of the host trees read here, swapped for fake trees in tests.
var (
	sysRoot  = "/sys"
	procRoot = "/proc"
	devRoot  = "/dev"
	idsDirs  = []string{"/usr/share/hwdata", "/usr/share/misc", "/var/lib/usbutils"}
)

// Device is one host USB or PCI device.
type Device struct {
	ID        string // what --device takes, e.g. "usb:3-2.1" or "pci:0000:01:00.0"
	Bus       string // "usb" | "pci"
	Addr      string // sysfs name, e.g. "3-2.1" or "0000:01:00.0"
	VendorID  string
	ProductID string
	Name      string
	Driver    string

	IOMMUGroup   int      // PCI only, -1 without an IOMMU
	GroupMembers []string // PCI only: the other endpoints of its IOMMU group

	Blocked string // why it can't be passed, empty if it can
}

func read(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func linkBase(path string) string {
	dst, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(dst)
}

func usbDir(addr string) string { return filepath.Join(sysRoot, "bus/usb/devices", addr) }
func pciDir(addr string) string { return filepath.Join(sysRoot, "bus/pci/devices", addr) }

// List returns every USB device (hubs left out) and every PCI device (bridges left out).
func List() ([]Device, error) {
	crit := criticalPaths()
	names := loadIDs("usb.ids")
	var out []Device

	usb, err := os.ReadDir(filepath.Join(sysRoot, "bus/usb/devices"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range usb {
		addr := e.Name()
		dir := usbDir(addr)
		// Interfaces have a ":" and root hubs are "usbN"; hubs can't be passed usefully.
		if strings.Contains(addr, ":") || strings.HasPrefix(addr, "usb") || read(dir+"/bDeviceClass") == "09" {
			continue
		}
		d := Device{
			ID: "usb:" + addr, Bus: "usb", Addr: addr,
			VendorID: read(dir + "/idVendor"), ProductID: read(dir + "/idProduct"), IOMMUGroup: -1,
		}
		d.Name = names.name(d.VendorID, d.ProductID)
		if d.Name == "" {
			d.Name = strings.TrimSpace(read(dir+"/manufacturer") + " " + read(dir+"/product"))
		}
		d.Driver = usbDrivers(dir, addr)
		d.Blocked = crit.blocked(dir)
		out = append(out, d)
	}

	names = loadIDs("pci.ids")
	pci, err := os.ReadDir(filepath.Join(sysRoot, "bus/pci/devices"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range pci {
		addr := e.Name()
		dir := pciDir(addr)
		if isBridge(addr) {
			continue
		}
		d := Device{
			ID: "pci:" + addr, Bus: "pci", Addr: addr,
			VendorID:  strings.TrimPrefix(read(dir+"/vendor"), "0x"),
			ProductID: strings.TrimPrefix(read(dir+"/device"), "0x"),
			Driver:    linkBase(dir + "/driver"), IOMMUGroup: -1,
		}
		d.Name = names.name(d.VendorID, d.ProductID)
		if g := linkBase(dir + "/iommu_group"); g != "" {
			d.IOMMUGroup, _ = strconv.Atoi(g)
			members := groupEndpoints(addr)
			for _, m := range members {
				if m != addr {
					d.GroupMembers = append(d.GroupMembers, m)
				}
			}
			for _, m := range members {
				if d.Blocked = crit.blocked(pciDir(m)); d.Blocked != "" {
					if m != addr {
						d.Blocked += " (same IOMMU group: " + m + ")"
					}
					break
				}
			}
		} else {
			d.Blocked = "no IOMMU group (IOMMU off or missing)"
		}
		out = append(out, d)
	}
	return out, nil
}

// usbDrivers names the drivers bound to a USB device's interfaces, e.g. "usbhid".
func usbDrivers(dir, addr string) string {
	ifaces, _ := filepath.Glob(filepath.Join(dir, addr+":*"))
	var drivers []string
	for _, i := range ifaces {
		if d := linkBase(i + "/driver"); d != "" && !slices.Contains(drivers, d) {
			drivers = append(drivers, d)
		}
	}
	return strings.Join(drivers, ",")
}

// isBridge is true for a PCI-to-PCI bridge, which stays on the host bound to pcieport.
func isBridge(addr string) bool {
	return strings.HasPrefix(read(pciDir(addr)+"/class"), "0x0604")
}

// groupEndpoints returns every non-bridge device in addr's IOMMU group, addr included.
func groupEndpoints(addr string) []string {
	entries, err := os.ReadDir(filepath.Join(pciDir(addr), "iommu_group/devices"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !isBridge(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// Resolve finds the device a ParseDevice id points at, returning its bus and sysfs name.
func Resolve(id string) (bus, addr string, err error) {
	bus, rest, _ := strings.Cut(id, ":")
	switch bus {
	case "pci":
		if _, err := os.Stat(pciDir(rest)); err != nil {
			return "", "", fmt.Errorf("hostdev: %s is not on this host", id)
		}
		if linkBase(pciDir(rest)+"/iommu_group") == "" {
			return "", "", fmt.Errorf("hostdev: %s has no IOMMU group (turn on the IOMMU, e.g. intel_iommu=on)", id)
		}
		return bus, rest, nil
	case "usb":
		if !strings.Contains(rest, ":") {
			if _, err := os.Stat(usbDir(rest)); err != nil {
				return "", "", fmt.Errorf("hostdev: %s is not plugged in", id)
			}
			return bus, rest, nil
		}
		vendor, product, _ := strings.Cut(rest, ":")
		entries, _ := os.ReadDir(filepath.Join(sysRoot, "bus/usb/devices"))
		var found []string
		for _, e := range entries {
			dir := usbDir(e.Name())
			if !strings.Contains(e.Name(), ":") && read(dir+"/idVendor") == vendor && read(dir+"/idProduct") == product {
				found = append(found, e.Name())
			}
		}
		switch len(found) {
		case 0:
			return "", "", fmt.Errorf("hostdev: %s is not plugged in", id)
		case 1:
			return bus, found[0], nil
		}
		return "", "", fmt.Errorf("hostdev: %s matches %d devices (%s), use the usb:<bus>-<port> form instead",
			id, len(found), strings.Join(found, ", "))
	}
	return "", "", fmt.Errorf("hostdev: unknown device %q", id)
}

// Members returns the sysfs names a claim of addr takes: the whole IOMMU group for PCI.
func Members(bus, addr string) []string {
	if bus == "pci" {
		return groupEndpoints(addr)
	}
	return []string{addr}
}

// critical maps the sysfs path of each device the host can't lose to the reason.
type critical map[string]string

// blocked says why the device at dir can't be passed: it is, or sits above, a critical device.
func (c critical) blocked(dir string) string {
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	for path, why := range c {
		if path == p || strings.HasPrefix(path, p+"/") {
			return why
		}
	}
	return ""
}

// criticalPaths finds the devices behind the root filesystem, the default route and the boot display.
// ponytail: IPv4 default route only, add /proc/net/ipv6_route if IPv6-only hosts show up.
func criticalPaths() critical {
	c := critical{}
	add := func(path, why string) {
		if p, err := filepath.EvalSymlinks(path); err == nil {
			if _, ok := c[p]; !ok {
				c[p] = why
			}
		}
	}
	if blk := rootBlockDevice(); blk != "" {
		for _, b := range blockLeaves(blk) {
			add(b, "holds the root filesystem")
		}
	}
	if iface := defaultRouteIface(); iface != "" {
		for _, n := range netLeaves(iface) {
			add(filepath.Join(sysRoot, "class/net", n, "device"), "carries the default route")
		}
	}
	gpus, _ := filepath.Glob(filepath.Join(sysRoot, "bus/pci/devices/*/boot_vga"))
	for _, g := range gpus {
		if read(g) == "1" {
			add(filepath.Dir(g), "drives the host display")
		}
	}
	return c
}

// rootBlockDevice returns the /sys/class/block path of the device mounted on "/".
func rootBlockDevice() string {
	f, err := os.Open(filepath.Join(procRoot, "self/mountinfo"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := slices.Index(fields, "-")
		if len(fields) < 5 || fields[4] != "/" || sep < 0 || sep+2 >= len(fields) {
			continue
		}
		// btrfs reports an anonymous 0:N, so the mount source is the fallback.
		if !strings.HasPrefix(fields[2], "0:") {
			if p := filepath.Join(sysRoot, "dev/block", fields[2]); exists(p) {
				return p
			}
		}
		src := fields[sep+2]
		if strings.HasPrefix(src, "/dev/") {
			if real, err := filepath.EvalSymlinks(filepath.Join(devRoot, strings.TrimPrefix(src, "/dev/"))); err == nil {
				src = real
			}
			if p := filepath.Join(sysRoot, "class/block", filepath.Base(src)); exists(p) {
				return p
			}
		}
	}
	return ""
}

// blockLeaves follows dm/md "slaves" down to the real disks under blk.
func blockLeaves(blk string) []string {
	slaves, _ := os.ReadDir(filepath.Join(blk, "slaves"))
	if len(slaves) == 0 {
		return []string{blk}
	}
	var out []string
	for _, s := range slaves {
		out = append(out, blockLeaves(filepath.Join(sysRoot, "class/block", s.Name()))...)
	}
	return out
}

func defaultRouteIface() string {
	f, err := os.Open(filepath.Join(procRoot, "net/route"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 7 && fields[1] == "00000000" && fields[7] == "00000000" {
			return fields[0]
		}
	}
	return ""
}

// netLeaves follows bridge, bond and vlan "lower_" links down to the real NICs under iface.
func netLeaves(iface string) []string {
	lowers, _ := filepath.Glob(filepath.Join(sysRoot, "class/net", iface, "lower_*"))
	if len(lowers) == 0 {
		return []string{iface}
	}
	var out []string
	for _, l := range lowers {
		out = append(out, netLeaves(strings.TrimPrefix(filepath.Base(l), "lower_"))...)
	}
	return out
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ids maps "vendor" and "vendor:product" to names from a usb.ids/pci.ids file.
type ids map[string]string

func (n ids) name(vendor, product string) string {
	if d := n[vendor+":"+product]; d != "" {
		return n[vendor] + " " + d
	}
	return n[vendor]
}

func loadIDs(file string) ids {
	n := ids{}
	for _, dir := range idsDirs {
		f, err := os.Open(filepath.Join(dir, file))
		if err != nil {
			continue
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		vendor := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case len(line) > 6 && isHex4(line[:4]) && line[4:6] == "  ":
				vendor = line[:4]
				n[vendor] = line[6:]
			case len(line) > 7 && line[0] == '\t' && isHex4(line[1:5]) && line[5:7] == "  " && vendor != "":
				n[vendor+":"+line[1:5]] = line[7:]
			case len(line) > 0 && line[0] != '\t' && line[0] != '#':
				vendor = "" // classes and other sections after the vendor list
			}
		}
		return n
	}
	return n
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	_, err := strconv.ParseUint(s, 16, 16)
	return err == nil && strings.ToLower(s) == s
}
