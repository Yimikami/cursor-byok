package cursor

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Match Cursor's --user-data-dir without moving the BYOK config, API keys or
// certificate. This is useful for portable profiles and isolated native tests.
func cursorDataDirOverride() (string, bool) {
	root, set := os.LookupEnv("CURSOR_BYOK_CURSOR_DATA_DIR")
	if !set {
		return "", false
	}
	if !filepath.IsAbs(root) {
		return "", true
	}
	return filepath.Clean(root), true
}

// ProfileBackupName keeps one profile's auth/settings sidecars from being
// replayed into another profile. The default profile retains legacy filenames.
func ProfileBackupName(name string) string {
	root, set := cursorDataDirOverride()
	if !set {
		return name
	}
	root = canonicalCursorProfileRoot(root)
	if defaultRoot := defaultCursorProfileRoot(); root != "" && defaultRoot != "" && root == canonicalCursorProfileRoot(defaultRoot) {
		return name
	}
	digest := sha256.Sum256([]byte(root))
	ext := filepath.Ext(name)
	return fmt.Sprintf("%s-%x%s", strings.TrimSuffix(name, ext), digest[:8], ext)
}

func canonicalCursorProfileRoot(root string) string {
	if root == "" {
		return ""
	}
	root = filepath.Clean(root)
	// Existing symlink/junction aliases address the same data and therefore
	// must recover from the same backup. For a profile not created yet, keep
	// its cleaned absolute spelling; ordinary profile creation is stable.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if runtime.GOOS == "windows" {
		root = strings.ToLower(root)
	}
	return root
}

// Keep this default in step with settingsPath/stateDBPath. Resolving it must
// not temporarily mutate the process environment while another request runs.
func defaultCursorProfileRoot() string {
	if runtime.GOOS == "windows" {
		if root := os.Getenv("APPDATA"); root != "" {
			return filepath.Join(root, "Cursor")
		}
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Cursor")
	}
	return filepath.Join(home, ".config", "Cursor")
}
