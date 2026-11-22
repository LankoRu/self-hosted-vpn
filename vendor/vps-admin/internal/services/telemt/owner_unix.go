//go:build unix

package telemt

import (
	"os"
	"syscall"
)

// fileOwner — uid/gid файла (сохраняем владельца при атомарной замене конфига).
func fileOwner(st os.FileInfo) (uid, gid int, ok bool) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid), int(sys.Gid), true
	}
	return 0, 0, false
}
