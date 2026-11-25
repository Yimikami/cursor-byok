//go:build darwin

package cursor

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// SystemConfiguration stores services under stable UUIDs. Unlike the active
// networksetup list, this includes disabled services and other locations, so
// absence can be distinguished from a rename or a location change.
// NetworkServices, CurrentSet, and Sets are public SystemConfiguration keys.
type darwinNetworkInventory struct {
	services map[string]string // service UUID -> current name, across all locations
	current  map[string]bool   // includes disabled services in the current location
}

func readDarwinNetworkInventory() (darwinNetworkInventory, error) {
	out, err := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", "/Library/Preferences/SystemConfiguration/preferences.plist").CombinedOutput()
	if err != nil {
		return darwinNetworkInventory{}, fmt.Errorf("read macOS network service identities: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	inventory, err := parseDarwinNetworkInventory(out)
	if err != nil {
		return darwinNetworkInventory{}, err
	}
	ids := make([]string, 0, len(inventory.services))
	for id := range inventory.services {
		ids = append(ids, id)
	}
	names, err := readDarwinNativeServiceNames(ids)
	if err != nil {
		return darwinNetworkInventory{}, err
	}
	return inventory.withNativeServiceNames(names), nil
}

func parseDarwinNetworkInventory(data []byte) (darwinNetworkInventory, error) {
	var prefs struct {
		NetworkServices map[string]json.RawMessage `json:"NetworkServices"`
		CurrentSet      string                     `json:"CurrentSet"`
		Sets            map[string]struct {
			Network struct {
				Service map[string]json.RawMessage `json:"Service"`
			} `json:"Network"`
		} `json:"Sets"`
	}
	if err := json.Unmarshal(data, &prefs); err != nil {
		return darwinNetworkInventory{}, fmt.Errorf("decode macOS network inventory: %w", err)
	}
	if prefs.NetworkServices == nil || prefs.Sets == nil || !strings.HasPrefix(prefs.CurrentSet, "/Sets/") {
		return darwinNetworkInventory{}, fmt.Errorf("macOS network inventory is incomplete; recovery entries retained")
	}
	set, ok := prefs.Sets[strings.TrimPrefix(prefs.CurrentSet, "/Sets/")]
	if !ok || set.Network.Service == nil {
		return darwinNetworkInventory{}, fmt.Errorf("current macOS network location is unavailable; recovery entries retained")
	}
	result := darwinNetworkInventory{services: make(map[string]string), current: make(map[string]bool)}
	for id := range prefs.NetworkServices {
		result.services[id] = ""
	}
	for id := range set.Network.Service {
		if _, ok := result.services[id]; !ok {
			return darwinNetworkInventory{}, fmt.Errorf("macOS network inventory references unknown service %s; retry recovery", id)
		}
		result.current[id] = true
	}
	return result, nil
}

// SCNetworkServiceGetName supplies the same localized/fallback names used by
// networksetup. Neither stored service names nor interface aliases reliably
// reproduce them. Resolve by UUID only; an unavailable native name leaves that
// identity unresolved without blocking recovery of unrelated services.
func (i darwinNetworkInventory) withNativeServiceNames(names map[string]string) darwinNetworkInventory {
	resolved := make(map[string]string, len(i.services))
	for id := range i.services {
		resolved[id] = names[id]
	}
	i.services = resolved
	return i
}

func (i darwinNetworkInventory) idForName(name string) (string, error) {
	found := ""
	for id, currentName := range i.services {
		if !i.current[id] || currentName != name {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("multiple macOS network services are named %q; recovery entries retained", name)
		}
		found = id
	}
	if found == "" {
		return "", fmt.Errorf("network service %q has no identity in the current location", name)
	}
	return found, nil
}

func (i darwinNetworkInventory) restoreName(sp darwinServiceProxy) (name string, deleted bool, err error) {
	id := sp.ServiceID
	if id == "" {
		// Old backups contain only names. An absent name could be a rename,
		// so do not claim deletion and abandon a potentially active proxy.
		id, err = i.idForName(sp.Service)
		if err != nil {
			return "", false, fmt.Errorf("cannot safely resolve legacy backup for %q (it may have been renamed): %w", sp.Service, err)
		}
	}
	name, exists := i.services[id]
	if !exists {
		return "", true, nil
	}
	if !i.current[id] {
		return "", false, fmt.Errorf("network service %q (%s) is in another location; switch back before retrying recovery", sp.Service, id)
	}
	if name == "" {
		return "", false, fmt.Errorf("network service %s has no name; recovery entry retained", id)
	}
	resolved, err := i.idForName(name)
	if err != nil || resolved != id {
		return "", false, fmt.Errorf("network service name %q is ambiguous; recovery entry retained", name)
	}
	return name, false, nil
}
