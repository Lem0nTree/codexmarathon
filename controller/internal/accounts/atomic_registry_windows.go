//go:build windows

package accounts

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32RegistryMoveFile = syscall.NewLazyDLL("kernel32.dll")
	registryMoveFileEx       = kernel32RegistryMoveFile.NewProc("MoveFileExW")
)

const (
	registryMoveFileReplaceExisting = 0x00000001
	registryMoveFileWriteThrough    = 0x00000008
)

func replaceRegistryFile(source, destination string) error {
	sourcePtr, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPtr, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	result, _, callErr := registryMoveFileEx.Call(
		uintptr(unsafe.Pointer(sourcePtr)),
		uintptr(unsafe.Pointer(destinationPtr)),
		registryMoveFileReplaceExisting|registryMoveFileWriteThrough,
	)
	if result == 0 {
		if callErr != nil && callErr != syscall.Errno(0) {
			return callErr
		}
		return fmt.Errorf("MoveFileExW failed")
	}
	return nil
}

func syncRegistryDirectory(string) error {
	return nil
}
