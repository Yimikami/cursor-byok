package certs

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const caCommonName = "cursor-byok Local CA"

// InstallUserRoot adds the CA at certPath into the current-user trusted root
// store. Windows uses the user Root store, macOS uses the login keychain, and
// Linux uses the user's NSS DB when certutil is available.
func InstallUserRoot(certPath string) error {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("certutil", "-user", "-addstore", "Root", certPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("certutil: %w (%s)", err, trim(out))
		}
		return nil
	case "darwin":
		keychain, err := darwinLoginKeychain()
		if err != nil {
			return err
		}
		cmd := exec.Command("security", "add-trusted-cert", "-r", "trustRoot", "-k", keychain, certPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("security add-trusted-cert: %w (%s)", err, trim(out))
		}
		return nil
	case "linux":
		db, err := linuxEnsureNSSDB()
		if err != nil {
			return err
		}
		_ = exec.Command("certutil", "-D", "-d", db, "-n", caCommonName).Run()
		cmd := exec.Command("certutil", "-A", "-d", db, "-n", caCommonName, "-t", "C,,", "-i", certPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("certutil nss add: %w (%s)", err, trim(out))
		}
		return nil
	default:
		return errors.New("CA auto-install is not supported on this platform")
	}
}

// UninstallUserRoot removes our CA from the current-user trusted root store.
func UninstallUserRoot(certPaths ...string) error {
	certPath, err := activeCertPath(certPaths)
	if err != nil {
		return err
	}
	cert, err := readRootCertificate(certPath)
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("certutil", "-user", "-delstore", "Root", certificateStoreID(cert))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("certutil: %w (%s)", err, trim(out))
		}
		return nil
	case "darwin":
		return uninstallDarwinRoot(certPath, cert, queryDarwinTrust, func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		})
	case "linux":
		db, err := linuxNSSDBPath()
		if err != nil {
			return err
		}
		if _, err := exec.LookPath("certutil"); err != nil {
			return nil
		}
		installed, err := exec.Command("certutil", "-L", "-d", db, "-n", caCommonName, "-a").CombinedOutput()
		if err != nil {
			msg := strings.ToLower(string(installed))
			if strings.Contains(msg, "could not find cert") || strings.Contains(msg, "not found") {
				return nil
			}
			return fmt.Errorf("read NSS certificate before removal: %w (%s)", err, trim(installed))
		}
		block, _ := pem.Decode(installed)
		if block == nil || !bytes.Equal(block.Bytes, cert.Raw) {
			return errors.New("NSS certificate with this name does not match the active CA; it was not removed")
		}
		cmd := exec.Command("certutil", "-D", "-d", db, "-n", caCommonName)
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := string(out)
			if strings.Contains(strings.ToLower(msg), "could not find cert") || strings.Contains(strings.ToLower(msg), "not found") {
				return nil
			}
			return fmt.Errorf("certutil nss delete: %w (%s)", err, trim(out))
		}
		return nil
	default:
		return errors.New("CA uninstall is not supported on this platform")
	}
}

// IsInstalledUserRoot checks the actual CA certificate, not its common name.
// With no path it uses the application's normal config directory. A stale root
// from a previous install must not make a newly generated CA look trusted.
func IsInstalledUserRoot(certPaths ...string) bool {
	certPath, err := activeCertPath(certPaths)
	if err != nil {
		return false
	}
	return isInstalledUserRoot(runtime.GOOS, certPath, runCertCheck)
}

func activeCertPath(paths []string) (string, error) {
	if len(paths) > 0 {
		return paths[0], nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cursor-byok", "ca", "ca.crt"), nil
}

func readRootCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("invalid CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return cert, nil
}

// SHA-1 is the Windows/macOS certificate-store identifier, not a signature.
func certificateStoreID(cert *x509.Certificate) string {
	sum := sha1.Sum(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func runCertCheck(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func isInstalledUserRoot(platform, certPath string, run func(string, ...string) ([]byte, error)) bool {
	cert, err := readRootCertificate(certPath)
	if err != nil || !cert.IsCA || time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
		return false
	}
	switch platform {
	case "windows":
		_, err := run("certutil", "-user", "-store", "Root", certificateStoreID(cert))
		return err == nil
	case "darwin":
		return isDarwinRootTrusted(certPath, cert, queryDarwinTrust, run)
	case "linux":
		db, err := linuxNSSDBPath()
		if err != nil {
			return false
		}
		out, err := run("certutil", "-L", "-d", db, "-n", caCommonName, "-a")
		if err != nil {
			return false
		}
		block, _ := pem.Decode(out)
		if block == nil || !bytes.Equal(block.Bytes, cert.Raw) {
			return false
		}
		_, err = run("certutil", "-V", "-d", db, "-n", caCommonName, "-u", "L")
		return err == nil
	default:
		return false
	}
}

func darwinLoginKeychain() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, "Library", "Keychains", "login.keychain-db"), nil
}

func linuxNSSDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return "sql:" + filepath.Join(home, ".pki", "nssdb"), nil
}

func linuxEnsureNSSDB() (string, error) {
	if _, err := exec.LookPath("certutil"); err != nil {
		return "", errors.New("Linux CA import requires certutil (usually from libnss3-tools / nss-tools); install it or follow the manual system-store steps shown in the UI")
	}
	db, err := linuxNSSDBPath()
	if err != nil {
		return "", err
	}
	dir := strings.TrimPrefix(db, "sql:")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "cert9.db")); os.IsNotExist(err) {
		cmd := exec.Command("certutil", "-N", "-d", db, "--empty-password")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("certutil init nss db: %w (%s)", err, trim(out))
		}
	}
	return db, nil
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
