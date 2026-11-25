//go:build darwin

package cursor

import "testing"

// This only reads configuration. CI on macOS checks the actual inventory parser
// and name mapping without changing trust, network services or proxy settings.
func TestDarwinNetworkServiceInventoryMatchesNetworksetup(t *testing.T) {
	inventory, err := readDarwinNetworkInventory()
	if err != nil {
		t.Fatal(err)
	}
	names, err := listNetworkServices()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := currentNetworkService(name, inventory.identities()); err != nil {
			t.Fatal(err)
		}
	}
}
