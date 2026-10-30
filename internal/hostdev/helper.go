package hostdev

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Claim records one device handed to a VM: what QEMU gets and what release must undo.
type Claim struct {
	Bus     string   // "usb" | "pci"
	Addr    string   // the requested device's sysfs name
	Members []Member // usb: the device itself; pci: every endpoint of its IOMMU group
	Node    string   // device file chowned to anvild's user
}

// Member is one device a claim took, with the host driver to give it back to.
type Member struct {
	Addr   string
	Driver string
}

type request struct {
	Op    string // "claim" | "release" | "netns-create" | "netns-delete"
	Bus   string
	Addr  string
	Claim *Claim
	Netns *Netns
}

type reply struct {
	Claim *Claim
	Error string
}

var (
	usbAddrRe = regexp.MustCompile(`^[0-9]+-[0-9]+(\.[0-9]+)*$`)
	pciRe     = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	driverRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// Client talks to the root helper listening on Socket.
type Client struct{ Socket string }

func (c Client) call(ctx context.Context, req request) (*Claim, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, fmt.Errorf("hostdev: reaching the device helper at %s (is anvil-devhelper.socket active?): %w", c.Socket, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("hostdev: sending to the device helper: %w", err)
	}
	var rep reply
	if err := json.NewDecoder(conn).Decode(&rep); err != nil {
		return nil, fmt.Errorf("hostdev: reading the device helper's reply: %w", err)
	}
	if rep.Error != "" {
		return nil, fmt.Errorf("hostdev: %s", rep.Error)
	}
	return rep.Claim, nil
}

// Claim hands a device (for PCI, its whole IOMMU group) over to anvild's user.
func (c Client) Claim(ctx context.Context, bus, addr string) (Claim, error) {
	cl, err := c.call(ctx, request{Op: "claim", Bus: bus, Addr: addr})
	if err != nil {
		return Claim{}, err
	}
	if cl == nil {
		return Claim{}, fmt.Errorf("hostdev: the device helper sent an empty claim")
	}
	return *cl, nil
}

// Release gives a claimed device back to the host.
func (c Client) Release(ctx context.Context, cl Claim) error {
	_, err := c.call(ctx, request{Op: "release", Claim: &cl})
	return err
}

// handle runs one helper request as root on behalf of the peer uid and gid.
func handle(req request, uid, gid int) reply {
	var (
		cl  Claim
		err error
	)
	switch req.Op {
	case "claim":
		cl, err = claim(req.Bus, req.Addr, uid)
	case "release":
		if req.Claim == nil {
			err = fmt.Errorf("release without a claim")
		} else {
			err = release(*req.Claim)
		}
	case "netns-create", "netns-delete":
		switch {
		case req.Netns == nil:
			err = fmt.Errorf("%s without a netns", req.Op)
		case req.Op == "netns-create":
			err = createNetns(*req.Netns, uid, gid)
		default:
			err = deleteNetns(req.Netns.ID)
		}
	default:
		err = fmt.Errorf("unknown op %q", req.Op)
	}
	if err != nil {
		return reply{Error: err.Error()}
	}
	return reply{Claim: &cl}
}

func claim(bus, addr string, uid int) (Claim, error) {
	switch {
	case bus == "usb" && usbAddrRe.MatchString(addr):
		return claimUSB(addr, uid)
	case bus == "pci" && pciRe.MatchString(addr):
		return claimPCI(addr, uid)
	}
	return Claim{}, fmt.Errorf("invalid device %s:%s", bus, addr)
}

func claimUSB(addr string, uid int) (Claim, error) {
	dir := usbDir(addr)
	if !exists(dir) {
		return Claim{}, fmt.Errorf("usb:%s is not plugged in", addr)
	}
	if why := criticalPaths().blocked(dir); why != "" {
		return Claim{}, fmt.Errorf("usb:%s %s and can't be passed to a VM", addr, why)
	}
	node, err := usbNode(addr)
	if err != nil {
		return Claim{}, err
	}
	if err := os.Chown(node, uid, -1); err != nil {
		return Claim{}, err
	}
	return Claim{Bus: "usb", Addr: addr, Members: []Member{{Addr: addr}}, Node: node}, os.Chmod(node, 0o600)
}

func usbNode(addr string) (string, error) {
	bus, err1 := atoiRead(usbDir(addr) + "/busnum")
	dev, err2 := atoiRead(usbDir(addr) + "/devnum")
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("usb:%s: reading its bus and device number", addr)
	}
	return filepath.Join(devRoot, fmt.Sprintf("bus/usb/%03d/%03d", bus, dev)), nil
}

func atoiRead(path string) (int, error) {
	var n int
	_, err := fmt.Sscan(read(path), &n)
	return n, err
}

func claimPCI(addr string, uid int) (cl Claim, err error) {
	group := linkBase(pciDir(addr) + "/iommu_group")
	if group == "" {
		return Claim{}, fmt.Errorf("pci:%s has no IOMMU group", addr)
	}
	members := groupEndpoints(addr)
	crit := criticalPaths()
	for _, m := range members {
		if why := crit.blocked(pciDir(m)); why != "" {
			return Claim{}, fmt.Errorf("pci:%s %s and can't be passed to a VM (IOMMU group %s)", m, why, group)
		}
	}
	if !exists(filepath.Join(sysRoot, "bus/pci/drivers/vfio-pci")) {
		if out, err := exec.Command("modprobe", "vfio-pci").CombinedOutput(); err != nil {
			return Claim{}, fmt.Errorf("loading vfio-pci: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	cl = Claim{Bus: "pci", Addr: addr, Node: filepath.Join(devRoot, "vfio", group)}
	defer func() {
		if err != nil {
			_ = release(cl)
		}
	}()
	for _, m := range members {
		drv := linkBase(pciDir(m) + "/driver")
		cl.Members = append(cl.Members, Member{Addr: m, Driver: drv})
		if drv == "vfio-pci" {
			continue
		}
		if err := bindVFIO(m, drv); err != nil {
			return cl, err
		}
	}
	if err := waitFor(cl.Node, 3*time.Second); err != nil {
		return cl, err
	}
	return cl, os.Chown(cl.Node, uid, -1)
}

func bindVFIO(addr, drv string) error {
	dir := pciDir(addr)
	if err := writeSys(dir+"/driver_override", "vfio-pci"); err != nil {
		return err
	}
	if drv != "" {
		if err := writeSys(dir+"/driver/unbind", addr); err != nil {
			return err
		}
	}
	if err := writeSys(filepath.Join(sysRoot, "bus/pci/drivers_probe"), addr); err != nil {
		return err
	}
	if got := linkBase(dir + "/driver"); got != "vfio-pci" {
		return fmt.Errorf("pci:%s did not bind to vfio-pci (driver now %q)", addr, got)
	}
	return nil
}

// release gives a claim's devices back. Only addresses and driver names are taken from
// the caller: ownership comes back to root and udev rules, never to a caller-chosen owner.
func release(cl Claim) error {
	switch {
	case cl.Bus == "usb" && usbAddrRe.MatchString(cl.Addr):
		node, err := usbNode(cl.Addr)
		if err != nil {
			return nil // unplugged meanwhile, its node is gone too
		}
		if err := os.Chown(node, 0, 0); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = os.Chmod(node, 0o664)
		_ = exec.Command("udevadm", "trigger", "--action=change", usbDir(cl.Addr)).Run()
		return nil
	case cl.Bus == "pci":
		var errs []string
		for _, m := range cl.Members {
			if !pciRe.MatchString(m.Addr) || (m.Driver != "" && !driverRe.MatchString(m.Driver)) {
				return fmt.Errorf("invalid claim member %q", m.Addr)
			}
		}
		for _, m := range cl.Members {
			if err := unbindVFIO(m); err != nil {
				errs = append(errs, err.Error())
			}
		}
		// The group file goes away once no member is on vfio-pci; a leftover gets root again.
		if group := linkBase(pciDir(cl.Addr) + "/iommu_group"); group != "" {
			_ = os.Chown(filepath.Join(devRoot, "vfio", group), 0, 0)
		}
		if len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return nil
	}
	return fmt.Errorf("invalid claim %s:%s", cl.Bus, cl.Addr)
}

func unbindVFIO(m Member) error {
	dir := pciDir(m.Addr)
	if !exists(dir) || m.Driver == "vfio-pci" {
		return nil
	}
	if linkBase(dir+"/driver") == "vfio-pci" {
		if err := writeSys(dir+"/driver/unbind", m.Addr); err != nil {
			return err
		}
	}
	if err := writeSys(dir+"/driver_override", "\n"); err != nil {
		return err
	}
	if m.Driver == "" {
		return nil
	}
	if bind := filepath.Join(sysRoot, "bus/pci/drivers", m.Driver, "bind"); exists(bind) {
		return writeSys(bind, m.Addr)
	}
	return writeSys(filepath.Join(sysRoot, "bus/pci/drivers_probe"), m.Addr)
}

func writeSys(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0o200); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func waitFor(path string, timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); ; time.Sleep(50 * time.Millisecond) {
		if exists(path) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear", path)
		}
	}
}
