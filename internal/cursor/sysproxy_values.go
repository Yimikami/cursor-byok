package cursor

import (
	"errors"
	"fmt"
	"os"
)

// sysProxyBackup records the Windows values we own, including absence.
// Keeping this logic independent of the registry allows failure-path tests
// without changing the host's network configuration.
type sysProxyBackup struct {
	HasEnable   bool   `json:"hasEnable"`
	Enable      uint32 `json:"enable,omitempty"`
	HasServer   bool   `json:"hasServer"`
	Server      string `json:"server,omitempty"`
	HasOverride bool   `json:"hasOverride"`
	Override    string `json:"override,omitempty"`
}

type proxyRegistry interface {
	GetIntegerValue(string) (uint64, uint32, error)
	GetStringValue(string) (string, uint32, error)
	SetDWordValue(string, uint32) error
	SetStringValue(string, string) error
	DeleteValue(string) error
}

func readProxyValues(k proxyRegistry) (sysProxyBackup, error) {
	var bk sysProxyBackup
	v, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil && !os.IsNotExist(err) {
		return bk, fmt.Errorf("read ProxyEnable: %w", err)
	}
	bk.HasEnable, bk.Enable = err == nil, uint32(v)
	bk.Server, _, err = k.GetStringValue("ProxyServer")
	if err != nil && !os.IsNotExist(err) {
		return bk, fmt.Errorf("read ProxyServer: %w", err)
	}
	bk.HasServer = err == nil
	bk.Override, _, err = k.GetStringValue("ProxyOverride")
	if err != nil && !os.IsNotExist(err) {
		return bk, fmt.Errorf("read ProxyOverride: %w", err)
	}
	bk.HasOverride = err == nil
	return bk, nil
}

func applyProxyValues(k proxyRegistry, addr string) error {
	if err := k.SetStringValue("ProxyServer", addr); err != nil {
		return err
	}
	if err := k.SetStringValue("ProxyOverride", "<local>"); err != nil {
		return err
	}
	// Enable only after the destination is ready.
	return k.SetDWordValue("ProxyEnable", 1)
}

func restoreProxyValues(k proxyRegistry, bk sysProxyBackup) error {
	remove := func(name string) error {
		err := k.DeleteValue(name)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	restoreString := func(name string, has bool, value string) error {
		if has {
			return k.SetStringValue(name, value)
		}
		return remove(name)
	}
	// First deactivate our destination. Attempt every value even when one
	// restore fails, and retain all errors so the caller keeps the backup.
	disableErr := k.SetDWordValue("ProxyEnable", 0)
	serverErr := restoreString("ProxyServer", bk.HasServer, bk.Server)
	overrideErr := restoreString("ProxyOverride", bk.HasOverride, bk.Override)
	if err := errors.Join(disableErr, serverErr, overrideErr); err != nil {
		return err
	}
	if bk.HasEnable {
		return k.SetDWordValue("ProxyEnable", bk.Enable)
	}
	return remove("ProxyEnable")
}
