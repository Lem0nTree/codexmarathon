//go:build windows

package credentials

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32MoveFile = syscall.NewLazyDLL("kernel32.dll")
	moveFileEx       = kernel32MoveFile.NewProc("MoveFileExW")
)

const (
	moveFileReplaceExisting = 0x00000001
	moveFileWriteThrough    = 0x00000008
)

func replaceFile(source, destination string) error {
	sourcePtr, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPtr, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileEx.Call(
		uintptr(unsafe.Pointer(sourcePtr)),
		uintptr(unsafe.Pointer(destinationPtr)),
		moveFileReplaceExisting|moveFileWriteThrough,
	)
	if result == 0 {
		if callErr != nil && callErr != syscall.Errno(0) {
			return callErr
		}
		return fmt.Errorf("MoveFileExW failed")
	}
	return nil
}

func syncDirectory(string) error {
	// Windows' MoveFileExW with MOVEFILE_WRITE_THROUGH provides the durable
	// rename request. Directory handles cannot be synced portably via os.File.
	return nil
}

func isPermissionMetadataError(error) bool {
	return false
}
