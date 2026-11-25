package cursor

import (
	"os"
	"path/filepath"
)

// Write the recovery record before changing system settings. In particular,
// adding a macOS service must not truncate the original crash-recovery record.
func writeSystemProxyBackup(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".sysproxy-backup-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
