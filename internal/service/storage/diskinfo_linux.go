//go:build linux

package storage

import (
	"fmt"
	"syscall"
)

// fsTypeName maps the statfs magic number to a file system name.
func fsTypeName(st syscall.Statfs_t) string {
	// #nosec G115 -- the bits of the filesystem magic number
	switch uint64(st.Type) {
	case 0xEF53:
		return "ext4"
	case 0x58465342:
		return "xfs"
	case 0x9123683E:
		return "btrfs"
	case 0x01021994:
		return "tmpfs"
	case 0x6969:
		return "nfs"
	case 0x2FC12FC1:
		return "zfs"
	case 0x794C7630:
		return "overlayfs"
	case 0xF15F:
		return "ecryptfs"
	case 0x65735546:
		return "fuse"
	default:
		return fmt.Sprintf("0x%x", uint64(st.Type)) // #nosec G115 -- the bits of the filesystem magic number
	}
}
