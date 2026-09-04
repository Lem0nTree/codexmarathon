//go:build !windows

package accounts

import "os"

func replaceRegistryFile(source, destination string) error {
	return os.Rename(source, destination)
}

func syncRegistryDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
