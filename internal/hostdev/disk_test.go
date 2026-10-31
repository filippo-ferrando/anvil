package hostdev

import (
	"path/filepath"
	"testing"
)

func TestCloneDiskValidate(t *testing.T) {
	orig := JailerRoot
	JailerRoot = "/var/lib/anvil/jailer"
	t.Cleanup(func() { JailerRoot = orig })
	good := CloneDisk{ID: "eph-01ABC", Key: "d1791f77aa", Jail: "/var/lib/anvil/jailer/firecracker/eph-01ABC/root"}
	if err := good.validate(); err != nil {
		t.Fatalf("valid disk refused: %v", err)
	}
	for name, d := range map[string]CloneDisk{
		"bad id":         {ID: "../etc", Key: good.Key, Jail: good.Jail},
		"bad key":        {ID: good.ID, Key: "../../etc", Jail: good.Jail},
		"jail outside":   {ID: good.ID, Key: good.Key, Jail: "/etc"},
		"jail escapes":   {ID: good.ID, Key: good.Key, Jail: "/var/lib/anvil/jailer/../../../etc"},
		"jail is root":   {ID: good.ID, Key: good.Key, Jail: "/var/lib/anvil/jailer"},
		"jail sibling":   {ID: good.ID, Key: good.Key, Jail: "/var/lib/anvil/jailer-other/x"},
		"jail relative":  {ID: good.ID, Key: good.Key, Jail: "firecracker/eph-01ABC/root"},
		"jail dotdot in": {ID: good.ID, Key: good.Key, Jail: filepath.Join(JailerRoot, "..")},
	} {
		if err := d.validate(); err == nil {
			t.Errorf("%s: %+v accepted", name, d)
		}
	}
	if DMName("eph-01ABC") != "anvil-eph-01ABC" {
		t.Errorf("DMName = %q", DMName("eph-01ABC"))
	}
}
