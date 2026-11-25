//go:build windows

package cursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

const inetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

const (
	internetOptionSettingsChanged = 39
	internetOptionRefresh         = 37
)

func sysProxyBackupPath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "sysproxy-backup.json")
}

func readCurrentSysProxy() (sysProxyBackup, error) {
	var bk sysProxyBackup
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return bk, err
	}
	defer k.Close()
	return readProxyValues(k)
}

func saveSysProxyBackup(bk sysProxyBackup) error {
	p := sysProxyBackupPath()
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

func loadSysProxyBackup() (sysProxyBackup, error) {
	var bk sysProxyBackup
	p := sysProxyBackupPath()
	if p == "" {
		return bk, errors.New("could not resolve sysproxy backup path")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return bk, err
	}
	return bk, json.Unmarshal(b, &bk)
}

// EnableSystemProxy points the current-user WinINET proxy at addr (host:port)
// and broadcasts the change so apps that watch for proxy updates pick it up
// immediately. The previous WinINET state (ProxyEnable, ProxyServer,
// ProxyOverride) is snapshotted to disk the first time Enable is called so
// DisableSystemProxy can fully restore any pre-existing corporate/custom
// proxy. If a backup already exists we leave it untouched — that file is
// the user's last-known-good state and must survive crash-restart cycles.
func EnableSystemProxy(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("invalid proxy address %q", addr)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("invalid proxy port %q", port)
	}
	// A corrupt or unreadable backup is not permission to overwrite it.
	if _, err := loadSysProxyBackup(); errors.Is(err, os.ErrNotExist) {
		bk, err := readCurrentSysProxy()
		if err != nil {
			return fmt.Errorf("read system proxy before override: %w", err)
		}
		if err := saveSysProxyBackup(bk); err != nil {
			return fmt.Errorf("snapshot system proxy: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read system proxy backup: %w", err)
	}

	k, _, err := registry.CreateKey(registry.CURRENT_USER, inetSettingsKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := applyProxyValues(k, addr); err != nil {
		// Restore before returning; otherwise bridge will stop the listener
		// while Windows can still point at the partially configured proxy.
		return errors.Join(err, DisableSystemProxy())
	}
	notifyInetSettingsChanged()
	return nil
}

// DisableSystemProxy restores only a recorded override. Without a backup we
// do not own the current settings, so stopping must not disable another proxy.
func DisableSystemProxy() error {
	bk, err := loadSysProxyBackup()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read system proxy backup: %w", err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := restoreProxyValues(k, bk); err != nil {
		notifyInetSettingsChanged()
		return err // Preserve the backup for a later retry.
	}
	notifyInetSettingsChanged()
	return os.Remove(sysProxyBackupPath())
}

// IsSystemProxyEnabled reports whether WinINET proxy is currently on AND
// pointed at our address.
func IsSystemProxyEnabled(addr string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	enabled, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enabled != 1 {
		return false
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return false
	}
	return server == addr
}

func notifyInetSettingsChanged() {
	wininet, err := syscall.LoadDLL("wininet.dll")
	if err != nil {
		return
	}
	defer wininet.Release()
	proc, err := wininet.FindProc("InternetSetOptionW")
	if err != nil {
		return
	}
	proc.Call(0, internetOptionSettingsChanged, uintptr(unsafe.Pointer(nil)), 0)
	proc.Call(0, internetOptionRefresh, uintptr(unsafe.Pointer(nil)), 0)
}
