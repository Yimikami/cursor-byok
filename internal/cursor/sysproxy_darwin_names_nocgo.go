//go:build darwin && !cgo

package cursor

import "fmt"

func readDarwinNativeServiceNames([]string) (map[string]string, error) {
	return nil, fmt.Errorf("reading native macOS network service names requires a CGO-enabled build; recovery entries retained")
}
