//go:build darwin && cgo

package cursor

import (
	"os"
	"testing"
)

// This opt-in check only reads the host inventory. Routine tests use fixtures;
// an explicit local check can compare the native API with networksetup without
// installing a certificate, enabling a proxy, or changing any service setting.
func TestDarwinNativeInventoryReadOnly(t *testing.T) {
	if os.Getenv("CURSOR_BYOK_READONLY_NATIVE_TEST") != "1" {
		t.Skip("set CURSOR_BYOK_READONLY_NATIVE_TEST=1 for read-only host inventory validation")
	}
	inventory, err := readDarwinNetworkInventory()
	if err != nil {
		t.Fatal(err)
	}
	names, err := listNetworkServiceNames(true)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	phoneListed, phoneResolved := false, false
	for index, name := range names {
		id, err := inventory.idForName(name)
		if err != nil {
			t.Errorf("listed service %d did not resolve to a unique native UUID", index)
		} else if id != "" {
			resolved++
		}
		if name == "iPhone USB" {
			phoneListed, phoneResolved = true, err == nil && id != ""
		}
	}
	t.Logf("listed=%d resolved=%d iPhoneUSBListed=%t iPhoneUSBResolved=%t", len(names), resolved, phoneListed, phoneResolved)
}
