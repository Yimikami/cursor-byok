package cursor

import "fmt"

// The name accepted by networksetup can change or be localized. The service
// identifier belongs to the SystemConfiguration record and survives renames.
// Inventory includes disabled services and services outside the current set.
type networkServiceIdentity struct {
	ID      string
	Name    string
	Current bool
}

func currentNetworkService(name string, services map[string]networkServiceIdentity) (networkServiceIdentity, error) {
	var match networkServiceIdentity
	for _, service := range services {
		if service.Current && service.Name == name {
			if match.ID != "" {
				return networkServiceIdentity{}, fmt.Errorf("network service name %q is ambiguous", name)
			}
			match = service
		}
	}
	if name == "" || match.ID == "" {
		return networkServiceIdentity{}, fmt.Errorf("cannot resolve network service %q in the current network location", name)
	}
	return match, nil
}

// Only absence from a successfully read complete preference dictionary proves
// deletion. An existing but disabled, renamed, malformed or non-current service
// must not be silently discarded. Legacy backups without an ID cannot safely
// distinguish a rename from deletion, so an unknown name remains recoverable.
func resolveNetworkService(id, savedName string, services map[string]networkServiceIdentity) (name string, deleted bool, err error) {
	if services == nil {
		return "", false, fmt.Errorf("network service inventory is unavailable")
	}
	if id == "" {
		service, err := currentNetworkService(savedName, services)
		if err != nil {
			return "", false, fmt.Errorf("legacy proxy backup has no service identifier: %w", err)
		}
		return service.Name, false, nil
	}
	service, exists := services[id]
	if !exists {
		return "", true, nil
	}
	if !service.Current {
		return "", false, fmt.Errorf("network service %q (%s) still exists outside the current network location", savedName, id)
	}
	matched, err := currentNetworkService(service.Name, services)
	if err != nil {
		return "", false, fmt.Errorf("cannot safely resolve saved network service %q (%s): %w", savedName, id, err)
	}
	if matched.ID != id {
		return "", false, fmt.Errorf("network service identity changed for %q (%s)", savedName, id)
	}
	return service.Name, false, nil
}
