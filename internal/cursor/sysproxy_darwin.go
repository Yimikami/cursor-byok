//go:build darwin

package cursor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// macOS exposes per-network-service proxy config through the `networksetup`
// CLI. We apply our HTTP + HTTPS proxy to every active (non-disabled)
// service so whichever interface the user actually routes traffic on
// (Wi-Fi, Ethernet, USB tether, …) ends up pointed at our MITM listener.
//
// Every service has independent state, so we snapshot the pre-change values
// for each one and restore them on Disable. The JSON sidecar lives next to
// the Windows equivalent in the cursor-byok config dir.

type darwinServiceProxy struct {
	Service   string `json:"service"`
	ServiceID string `json:"serviceId,omitempty"`
	// HTTPEnabled is the state of the "web proxy" (plain HTTP) setting; HTTPS
	// is tracked separately because macOS splits them into two controls.
	HTTPEnabled  bool   `json:"httpEnabled"`
	HTTPServer   string `json:"httpServer,omitempty"`
	HTTPPort     string `json:"httpPort,omitempty"`
	HTTPSEnabled bool   `json:"httpsEnabled"`
	HTTPSServer  string `json:"httpsServer,omitempty"`
	HTTPSPort    string `json:"httpsPort,omitempty"`
}

type darwinSysProxyBackup struct {
	Services []darwinServiceProxy `json:"services"`
}

func sysProxyBackupPath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "sysproxy-backup.json")
}

// listNetworkServices returns the names of every configured network service
// that isn't disabled. `networksetup -listallnetworkservices` prints a
// leading note line we strip, and prefixes disabled services with a "*".
func listNetworkServices() ([]string, error) {
	return listNetworkServiceNames(false)
}

func listNetworkServiceNames(includeDisabled bool) ([]string, error) {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil, fmt.Errorf("listallnetworkservices: %w", err)
	}
	var services []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if first {
			// Skip the banner: "An asterisk (*) denotes that a network
			// service is disabled."
			first = false
			if strings.Contains(strings.ToLower(line), "asterisk") {
				continue
			}
		}
		if strings.HasPrefix(line, "*") {
			if !includeDisabled {
				continue
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
		}
		services = append(services, line)
	}
	return services, scanner.Err()
}

// parseProxyGet decodes the 4-line response of
// `networksetup -getwebproxy <service>`:
//
//	Enabled: Yes
//	Server: 127.0.0.1
//	Port: 8080
//	Authenticated Proxy Enabled: 0
func parseProxyGet(out string) (enabled bool, server string, port string) {
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "Enabled:"):
			enabled = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "Enabled:")), "yes")
		case strings.HasPrefix(line, "Server:"):
			server = strings.TrimSpace(strings.TrimPrefix(line, "Server:"))
		case strings.HasPrefix(line, "Port:"):
			port = strings.TrimSpace(strings.TrimPrefix(line, "Port:"))
		}
	}
	return
}

func readServiceProxy(service string) (darwinServiceProxy, error) {
	sp := darwinServiceProxy{Service: service}
	out, err := exec.Command("networksetup", "-getwebproxy", service).Output()
	if err != nil {
		return sp, fmt.Errorf("read HTTP proxy for %s: %w", service, err)
	}
	sp.HTTPEnabled, sp.HTTPServer, sp.HTTPPort = parseProxyGet(string(out))
	out, err = exec.Command("networksetup", "-getsecurewebproxy", service).Output()
	if err != nil {
		return sp, fmt.Errorf("read HTTPS proxy for %s: %w", service, err)
	}
	sp.HTTPSEnabled, sp.HTTPSServer, sp.HTTPSPort = parseProxyGet(string(out))
	return sp, nil
}

func saveSysProxyBackup(bk darwinSysProxyBackup) error {
	return saveDarwinProxyBackup(sysProxyBackupPath(), bk)
}

func saveDarwinProxyBackup(p string, bk darwinSysProxyBackup) error {
	if p == "" {
		return errors.New("could not resolve sysproxy backup path")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(bk, "", "  ")
	if err != nil {
		return err
	}
	return writeSystemProxyBackup(p, b)
}

func loadSysProxyBackup() (darwinSysProxyBackup, error) {
	return loadDarwinProxyBackup(sysProxyBackupPath())
}

func loadDarwinProxyBackup(p string) (darwinSysProxyBackup, error) {
	var bk darwinSysProxyBackup
	if p == "" {
		return bk, errors.New("could not resolve sysproxy backup path")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return bk, err
	}
	return bk, json.Unmarshal(b, &bk)
}

// EnableSystemProxy points every active network service's HTTP + HTTPS proxy
// at addr (host:port). The first Enable call snapshots whatever proxy config
// was already in place to sysproxy-backup.json so DisableSystemProxy can
// fully restore it — including corporate / custom proxies the user had set
// before cursor-byok ran. A pre-existing backup is left untouched so a
// crash-restart cycle doesn't clobber the pristine state.
func EnableSystemProxy(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen addr %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %q", portStr)
	}
	services, err := listNetworkServices()
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return errors.New("no active macOS network services found")
	}

	inventory, err := readDarwinNetworkInventory()
	if err != nil {
		return err
	}

	// Validate the existing recovery record before touching any service.
	bk, err := loadSysProxyBackup()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read system proxy backup: %w", err)
	}
	known := make(map[string]bool)
	for index := range bk.Services {
		sp := &bk.Services[index]
		if sp.ServiceID == "" {
			sp.ServiceID, err = inventory.idForName(sp.Service)
			if err != nil {
				return err
			}
		}
		known[sp.ServiceID] = true
	}
	for _, svc := range services {
		id, err := inventory.idForName(svc)
		if err != nil {
			return err
		}
		if known[id] {
			continue
		}
		sp, err := readServiceProxy(svc)
		if err != nil {
			return err
		}
		sp.ServiceID = id
		bk.Services = append(bk.Services, sp)
	}
	if err := saveSysProxyBackup(bk); err != nil {
		return fmt.Errorf("snapshot system proxy: %w", err)
	}
	for _, svc := range services {
		if err := runNetworksetup("-setwebproxy", svc, host, strconv.Itoa(port)); err != nil {
			return errors.Join(err, DisableSystemProxy())
		}
		if err := runNetworksetup("-setsecurewebproxy", svc, host, strconv.Itoa(port)); err != nil {
			return errors.Join(err, DisableSystemProxy())
		}
	}
	return nil
}

// DisableSystemProxy restores only services recorded in the backup. Missing
// backups mean we own no settings; failures preserve the record for retry.
func DisableSystemProxy() error {
	return disableDarwinSystemProxyAt(sysProxyBackupPath(), readDarwinNetworkInventory, runNetworksetup)
}

func disableDarwinSystemProxyAt(path string, inventoryReader func() (darwinNetworkInventory, error), run func(...string) error) error {
	bk, err := loadDarwinProxyBackup(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read system proxy backup: %w", err)
	}
	inventory, err := inventoryReader()
	if err != nil {
		return err
	}
	remaining, err := restoreDarwinServiceProxies(bk, inventory, run)
	if len(remaining.Services) == 0 {
		return os.Remove(path)
	}
	// Persist only unresolved services. Confirmed deletions and completed
	// restorations must not be retried forever or overwrite later user edits.
	if saveErr := saveDarwinProxyBackup(path, remaining); saveErr != nil {
		return errors.Join(err, fmt.Errorf("save pending proxy recovery: %w", saveErr))
	}
	return err
}

func restoreDarwinServiceProxies(bk darwinSysProxyBackup, inventory darwinNetworkInventory, run func(...string) error) (darwinSysProxyBackup, error) {
	var remaining darwinSysProxyBackup
	var restoreErrors []error
	for _, sp := range bk.Services {
		name, deleted, err := inventory.restoreName(sp)
		if deleted {
			continue
		}
		if err != nil {
			remaining.Services = append(remaining.Services, sp)
			restoreErrors = append(restoreErrors, err)
			continue
		}
		var serviceErrors []error
		restore := func(setCommand, stateCommand, server, port string, enabled bool) {
			if server != "" && port != "" && port != "0" {
				if err := run(setCommand, name, server, port); err != nil {
					serviceErrors = append(serviceErrors, err)
					// Never reenable an unrecovered destination.
					enabled = false
				}
			}
			state := "off"
			if enabled {
				state = "on"
			}
			if err := run(stateCommand, name, state); err != nil {
				serviceErrors = append(serviceErrors, err)
			}
		}
		restore("-setwebproxy", "-setwebproxystate", sp.HTTPServer, sp.HTTPPort, sp.HTTPEnabled)
		restore("-setsecurewebproxy", "-setsecurewebproxystate", sp.HTTPSServer, sp.HTTPSPort, sp.HTTPSEnabled)
		if err := errors.Join(serviceErrors...); err != nil {
			remaining.Services = append(remaining.Services, sp)
			restoreErrors = append(restoreErrors, err)
		}
	}
	return remaining, errors.Join(restoreErrors...)
}

// IsSystemProxyEnabled reports whether at least one active network service
// is currently pointed at addr (host:port) on its HTTP web proxy. Used by
// the UI to reflect the running state; a false negative here is harmless.
func IsSystemProxyEnabled(addr string) bool {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	services, err := listNetworkServices()
	if err != nil {
		return false
	}
	for _, svc := range services {
		out, err := exec.Command("networksetup", "-getwebproxy", svc).Output()
		if err != nil {
			continue
		}
		enabled, server, port := parseProxyGet(string(out))
		if enabled && server == host && port == portStr {
			return true
		}
	}
	return false
}

// runNetworksetup invokes networksetup with the given args and returns a
// wrapped error that includes stderr so the UI surfaces actionable detail
// (most common cause: user lacks the permission to edit the System-level
// network service — solved by granting Full Disk Access or running the
// app the first time with admin rights).
func runNetworksetup(args ...string) error {
	cmd := exec.Command("networksetup", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("networksetup %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
