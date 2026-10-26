package hostdev

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeHost builds a small sysfs/proc/dev tree and points the package at it.
func fakeHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(path, content string) {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(path, target string) {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, target), p); err != nil {
			t.Fatal(err)
		}
	}
	const pci = "sys/devices/pci0000:00/"
	pciDev := func(dir, addr, class, vendor, device, group, driver string) {
		mk(dir+"/class", class)
		mk(dir+"/vendor", "0x"+vendor)
		mk(dir+"/device", "0x"+device)
		link("sys/bus/pci/devices/"+addr, dir)
		link(dir+"/iommu_group", "sys/kernel/iommu_groups/"+group)
		link("sys/kernel/iommu_groups/"+group+"/devices/"+addr, dir)
		if driver != "" {
			_ = os.MkdirAll(filepath.Join(root, "sys/bus/pci/drivers", driver), 0o755)
			link(dir+"/driver", "sys/bus/pci/drivers/"+driver)
		}
	}
	usbDev := func(dir, name, vendor, product, class, bus, dev string) {
		mk(dir+"/idVendor", vendor)
		mk(dir+"/idProduct", product)
		mk(dir+"/bDeviceClass", class)
		mk(dir+"/busnum", bus)
		mk(dir+"/devnum", dev)
		link("sys/bus/usb/devices/"+name, dir)
	}

	xhci := pci + "0000:00:14.0"
	pciDev(xhci, "0000:00:14.0", "0x0c0330", "8086", "a36d", "5", "xhci_hcd")
	usbDev(xhci+"/usb3", "usb3", "1d6b", "0003", "09", "3", "1")
	usbDev(xhci+"/usb3/3-1", "3-1", "05e3", "0610", "09", "3", "2")
	usbDev(xhci+"/usb3/3-2", "3-2", "046d", "c52b", "00", "3", "4")
	mk(xhci+"/usb3/3-2/3-2:1.0/bInterfaceClass", "03")
	_ = os.MkdirAll(filepath.Join(root, "sys/bus/usb/drivers/usbhid"), 0o755)
	link(xhci+"/usb3/3-2/3-2:1.0/driver", "sys/bus/usb/drivers/usbhid")
	link("sys/bus/usb/devices/3-2:1.0", xhci+"/usb3/3-2/3-2:1.0")
	usbDev(xhci+"/usb3/3-1/3-1.4", "3-1.4", "046d", "c52b", "00", "3", "5")
	mk("dev/bus/usb/003/004", "")

	// Root disk: an NVMe drive behind a bridge.
	bridge := pci + "0000:00:1d.0"
	pciDev(bridge, "0000:00:1d.0", "0x060400", "8086", "a330", "7", "pcieport")
	nvme := bridge + "/0000:01:00.0"
	pciDev(nvme, "0000:01:00.0", "0x010802", "144d", "a808", "8", "nvme")
	mk(nvme+"/nvme/nvme0/nvme0n1/nvme0n1p2/dev", "259:2")
	link("sys/dev/block/259:2", nvme+"/nvme/nvme0/nvme0n1/nvme0n1p2")
	mk("proc/self/mountinfo", "29 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw")

	pciDev(pci+"0000:00:02.0", "0000:00:02.0", "0x030000", "8086", "9bc4", "1", "i915")
	mk(pci+"0000:00:02.0/boot_vga", "1")

	// The default-route NIC shares group 9 with a USB controller.
	pciDev(pci+"0000:03:00.0", "0000:03:00.0", "0x020000", "8086", "15f3", "9", "igc")
	pciDev(pci+"0000:03:00.1", "0000:03:00.1", "0x0c0330", "1b21", "2142", "9", "xhci_hcd")
	link("sys/class/net/br0/lower_eth0", "sys/class/net/eth0")
	link("sys/class/net/eth0/device", pci+"0000:03:00.0")
	mk("proc/net/route", "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\n"+
		"br0\t00000000\t0102A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0")

	pciDev(pci+"0000:04:00.0", "0000:04:00.0", "0x030000", "10de", "1f82", "10", "nouveau")
	pciDev(pci+"0000:04:00.1", "0000:04:00.1", "0x040300", "10de", "10fa", "10", "snd_hda_intel")

	mk("ids/usb.ids", "# comment\n046d  Logitech, Inc.\n\tc52b  Unifying Receiver\nC 00  (Defined at Interface level)\n\t01  x")

	saved := []string{sysRoot, procRoot, devRoot}
	savedIDs := idsDirs
	sysRoot, procRoot, devRoot = filepath.Join(root, "sys"), filepath.Join(root, "proc"), filepath.Join(root, "dev")
	idsDirs = []string{filepath.Join(root, "ids")}
	t.Cleanup(func() {
		sysRoot, procRoot, devRoot = saved[0], saved[1], saved[2]
		idsDirs = savedIDs
	})
	return root
}

func TestList(t *testing.T) {
	fakeHost(t)
	devs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Device{}
	for _, d := range devs {
		byID[d.ID] = d
	}
	for _, skipped := range []string{"usb:usb3", "usb:3-1", "usb:3-2:1.0", "pci:0000:00:1d.0"} {
		if _, ok := byID[skipped]; ok {
			t.Errorf("%s should not be listed", skipped)
		}
	}
	if d := byID["usb:3-2"]; d.Name != "Logitech, Inc. Unifying Receiver" || d.Driver != "usbhid" || d.Blocked != "" {
		t.Errorf("usb:3-2 = %+v", d)
	}
	want := map[string]string{
		"pci:0000:01:00.0": "holds the root filesystem",
		"pci:0000:00:02.0": "drives the host display",
		"pci:0000:03:00.0": "carries the default route",
		"pci:0000:03:00.1": "carries the default route (same IOMMU group: 0000:03:00.0)",
		"pci:0000:04:00.0": "",
	}
	for id, why := range want {
		if got := byID[id].Blocked; got != why {
			t.Errorf("%s blocked = %q, want %q", id, got, why)
		}
	}
	if d := byID["pci:0000:04:00.0"]; d.IOMMUGroup != 10 || !slices.Equal(d.GroupMembers, []string{"0000:04:00.1"}) {
		t.Errorf("group of 0000:04:00.0 = %d %v", d.IOMMUGroup, d.GroupMembers)
	}
}

func TestResolve(t *testing.T) {
	fakeHost(t)
	if bus, addr, err := Resolve("usb:3-2"); err != nil || bus != "usb" || addr != "3-2" {
		t.Errorf("usb:3-2 = %s %s %v", bus, addr, err)
	}
	if _, _, err := Resolve("usb:046d:c52b"); err == nil || !strings.Contains(err.Error(), "3-1.4, 3-2") {
		t.Errorf("ambiguous vendor:product: got %v", err)
	}
	if _, _, err := Resolve("usb:1234:5678"); err == nil {
		t.Error("unplugged device resolved")
	}
	if _, _, err := Resolve("pci:0000:09:00.0"); err == nil {
		t.Error("missing PCI device resolved")
	}
	if got := Members("pci", "0000:04:00.1"); !slices.Equal(got, []string{"0000:04:00.0", "0000:04:00.1"}) {
		t.Errorf("members = %v", got)
	}
}

func TestClaimRefusesBlockedAndBadInput(t *testing.T) {
	fakeHost(t)
	for _, req := range []request{
		{Op: "claim", Bus: "pci", Addr: "0000:03:00.1"}, // shares a group with the default-route NIC
		{Op: "claim", Bus: "pci", Addr: "0000:01:00.0"}, // root disk
		{Op: "claim", Bus: "usb", Addr: "../../etc"},
		{Op: "release", Claim: &Claim{Bus: "pci", Addr: "0000:04:00.0", Members: []Member{{Addr: "0000:04:00.0", Driver: "../x"}}}},
		{Op: "release", Claim: &Claim{Bus: "disk", Addr: "sda"}},
		{Op: "format"},
	} {
		if rep := handle(req, os.Getuid()); rep.Error == "" {
			t.Errorf("%+v succeeded, want an error", req)
		}
	}
}

func TestClaimUSB(t *testing.T) {
	root := fakeHost(t)
	rep := handle(request{Op: "claim", Bus: "usb", Addr: "3-2"}, os.Getuid())
	if rep.Error != "" {
		t.Fatal(rep.Error)
	}
	node := filepath.Join(root, "dev/bus/usb/003/004")
	if rep.Claim.Node != node {
		t.Errorf("node = %s, want %s", rep.Claim.Node, node)
	}
	if fi, err := os.Stat(node); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("node mode = %v, %v", fi.Mode(), err)
	}
}
