package cursor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Cursor (and VSCode) settings.json keys we set so that Cursor honours the
// local proxy and uses the installed local CA with certificate validation. Stored on disk so we can
// restore the user's previous values on Stop.
type tweakBackup struct {
	RawValues              map[string]string `json:"rawValues,omitempty"`
	HasHTTPProxy           bool              `json:"hasHttpProxy"`
	HTTPProxy              any               `json:"httpProxy,omitempty"`
	HasHTTPProxyStrictSSL  bool              `json:"hasHttpProxyStrictSsl"`
	HTTPProxyStrictSSL     any               `json:"httpProxyStrictSsl,omitempty"`
	HasHTTPProxySupport    bool              `json:"hasHttpProxySupport"`
	HTTPProxySupport       any               `json:"httpProxySupport,omitempty"`
	HasHTTPProxyAuthHelper bool              `json:"hasHttpProxyAuthHelper"`
	HTTPProxyAuthHelper    any               `json:"httpProxyAuthHelper,omitempty"`
	HasHTTPCompatHTTP1     bool              `json:"hasHttpCompatHttp1"`
	HTTPCompatHTTP1        any               `json:"httpCompatHttp1,omitempty"`
	HasDisableHTTP2        bool              `json:"hasDisableHttp2"`
	DisableHTTP2           any               `json:"disableHttp2,omitempty"`
	HasProxyKerberos       bool              `json:"hasProxyKerberos"`
	ProxyKerberos          any               `json:"proxyKerberos,omitempty"`
}

var settingsMu sync.Mutex

// Status reports what we found in Cursor's settings.json so the UI can show
// the user exactly which keys are wired up.
type Status struct {
	Path            string `json:"path"`
	Found           bool   `json:"found"`
	Error           string `json:"error,omitempty"`
	ProxySet        bool   `json:"proxySet"`
	ProxyValue      string `json:"proxyValue,omitempty"`
	StrictSSLOff    bool   `json:"strictSSLOff"`
	ProxySupportOn  bool   `json:"proxySupportOn"`
	SystemCertsV2On bool   `json:"systemCertsV2On"`
	UseHTTP1        bool   `json:"useHttp1"`
	DisableHTTP2    bool   `json:"disableHttp2"`
	ProxyKerberos   bool   `json:"proxyKerberos"`
}

// GetStatus inspects settings.json and reports which tweaks are currently
// applied.
func GetStatus() Status {
	st := Status{Path: settingsPath()}
	if st.Path == "" {
		st.Error = "could not resolve Cursor settings path"
		return st
	}
	if _, err := os.Stat(st.Path); err != nil {
		if !os.IsNotExist(err) {
			st.Error = err.Error()
		}
		return st
	}
	st.Found = true
	settingsMu.Lock()
	defer settingsMu.Unlock()
	m, err := readSettings()
	if err != nil {
		st.Error = err.Error()
		return st
	}
	if v, ok := m["http.proxy"].(string); ok && v != "" {
		st.ProxySet = true
		st.ProxyValue = v
	}
	if v, ok := m["http.proxyStrictSSL"].(bool); ok && !v {
		st.StrictSSLOff = true
	}
	if v, ok := m["http.proxySupport"].(string); ok && v == "on" {
		st.ProxySupportOn = true
	}
	if v, ok := m["http.experimental.systemCertificatesV2"].(bool); ok && v {
		st.SystemCertsV2On = true
	}
	if v, ok := m["cursor.general.useHttp1"].(bool); ok && v {
		st.UseHTTP1 = true
	}
	if v, ok := m["cursor.general.disableHttp2"].(bool); ok && v {
		st.DisableHTTP2 = true
	}
	if v, ok := m["http.proxyKerberosServicePrincipal"].(string); ok && v != "" {
		st.ProxyKerberos = true
	}
	return st
}

// settingsPath returns the path to Cursor's user settings.json on the current
// platform. Returns "" when the directory doesn't exist (e.g. Cursor not
// installed for this user).
func settingsPath() string {
	if root, set := cursorDataDirOverride(); set {
		if root == "" {
			return ""
		}
		return filepath.Join(root, "User", "settings.json")
	}
	switch runtime.GOOS {
	case "windows":
		root := os.Getenv("APPDATA")
		if root == "" {
			return ""
		}
		return filepath.Join(root, "Cursor", "User", "settings.json")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "settings.json")
	default:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "Cursor", "User", "settings.json")
	}
}

func backupPath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, ProfileBackupName("cursor-settings-backup.json"))
}

func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cursor-byok"), nil
}

func readSettings() (map[string]any, error) {
	data, err := readSettingsData(settingsPath())
	if err != nil {
		return nil, err
	}
	m, err := decodeSettings(data)
	if err != nil {
		return nil, fmt.Errorf("settings.json is not valid JSONC: %w", err)
	}
	return m, nil
}

func readSettingsData(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("could not resolve Cursor settings path")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []byte("{}\n"), nil
	}
	return data, err
}

func writeSettingsData(path string, data []byte) error {
	if path == "" {
		return fmt.Errorf("could not resolve Cursor settings path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func cursorTweakChanges(proxyAddr string) map[string]settingsChange {
	return map[string]settingsChange{
		"http.proxy":                             {present: true, value: "http://" + proxyAddr},
		"http.proxyStrictSSL":                    {present: true, value: true},
		"http.proxySupport":                      {present: true, value: "on"},
		"http.experimental.systemCertificatesV2": {present: true, value: true},
		"cursor.general.useHttp1":                {present: true, value: true},
		"cursor.general.disableHttp2":            {present: true, value: true},
		"http.proxyKerberosServicePrincipal":     {present: true, value: "http://" + proxyAddr},
	}
}

// ApplyCursorTweaks writes the proxy + cert tweaks Cursor needs. Original
// values are saved so RevertCursorTweaks can put them back.
func ApplyCursorTweaks(proxyAddr string) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()

	return applyCursorTweaksAt(settingsPath(), backupPath(), proxyAddr)
}

func applyCursorTweaksAt(settingsFile, backupFile, proxyAddr string) error {
	data, err := readSettingsData(settingsFile)
	if err != nil {
		return err
	}
	doc, err := parseSettingsDocument(data)
	if err != nil {
		return fmt.Errorf("settings.json is not valid JSONC: %w", err)
	}
	changes := cursorTweakChanges(proxyAddr)
	rawValues, err := doc.rawValues(changes)
	if err != nil {
		return err
	}
	updated, err := editSettings(data, changes)
	if err != nil {
		return err
	}
	m := doc.values

	// Only snapshot when no backup exists yet. If the app crashed while our
	// tweaks were active, settings.json is already mutated — re-snapshotting
	// it would permanently overwrite the user's pristine values with our
	// overrides, and a later Revert would be a no-op. The file is removed
	// on successful Revert, so a clean cycle still re-snapshots next time.
	if p := backupFile; p != "" {
		if _, statErr := os.Stat(p); os.IsNotExist(statErr) {
			bk := tweakBackup{RawValues: rawValues}
			bk.HTTPProxy, bk.HasHTTPProxy = m["http.proxy"]
			bk.HTTPProxyStrictSSL, bk.HasHTTPProxyStrictSSL = m["http.proxyStrictSSL"]
			bk.HTTPProxySupport, bk.HasHTTPProxySupport = m["http.proxySupport"]
			bk.HTTPProxyAuthHelper, bk.HasHTTPProxyAuthHelper = m["http.experimental.systemCertificatesV2"]
			bk.HTTPCompatHTTP1, bk.HasHTTPCompatHTTP1 = m["cursor.general.useHttp1"]
			bk.DisableHTTP2, bk.HasDisableHTTP2 = m["cursor.general.disableHttp2"]
			bk.ProxyKerberos, bk.HasProxyKerberos = m["http.proxyKerberosServicePrincipal"]
			if err := saveBackupAt(backupFile, bk); err != nil {
				return err
			}
		} else if statErr != nil {
			return fmt.Errorf("inspect settings backup: %w", statErr)
		} else if _, err := loadBackupAt(backupFile); err != nil {
			return fmt.Errorf("read settings backup: %w", err)
		}
	} else {
		return fmt.Errorf("could not resolve settings backup path")
	}

	return writeSettingsData(settingsFile, updated)
}

// RevertCursorTweaks restores whatever was in settings.json before
// ApplyCursorTweaks ran.
func RevertCursorTweaks() error {
	settingsMu.Lock()
	defer settingsMu.Unlock()

	return revertCursorTweaksAt(settingsPath(), backupPath())
}

func revertCursorTweaksAt(settingsFile, backupFile string) error {
	bk, err := loadBackupAt(backupFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read settings backup: %w", err)
	}
	data, err := readSettingsData(settingsFile)
	if err != nil {
		return err
	}
	changes := make(map[string]settingsChange)
	restore := func(key string, has bool, val any) {
		changes[key] = settingsChange{present: has, value: val, raw: bk.RawValues[key]}
	}
	restore("http.proxy", bk.HasHTTPProxy, bk.HTTPProxy)
	restore("http.proxyStrictSSL", bk.HasHTTPProxyStrictSSL, bk.HTTPProxyStrictSSL)
	restore("http.proxySupport", bk.HasHTTPProxySupport, bk.HTTPProxySupport)
	restore("http.experimental.systemCertificatesV2", bk.HasHTTPProxyAuthHelper, bk.HTTPProxyAuthHelper)
	restore("cursor.general.useHttp1", bk.HasHTTPCompatHTTP1, bk.HTTPCompatHTTP1)
	restore("cursor.general.disableHttp2", bk.HasDisableHTTP2, bk.DisableHTTP2)
	restore("http.proxyKerberosServicePrincipal", bk.HasProxyKerberos, bk.ProxyKerberos)

	updated, err := editSettings(data, changes)
	if err != nil {
		return err
	}
	if err := writeSettingsData(settingsFile, updated); err != nil {
		return err
	}
	if err := os.Remove(backupFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove restored settings backup: %w", err)
	}
	return nil
}

func saveBackupAt(p string, bk tweakBackup) error {
	if p == "" {
		return fmt.Errorf("could not resolve backup path")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(bk, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func loadBackupAt(path string) (tweakBackup, error) {
	var bk tweakBackup
	b, err := os.ReadFile(path)
	if err != nil {
		return bk, err
	}
	return bk, json.Unmarshal(b, &bk)
}
