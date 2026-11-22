//go:build !unix

package telemt

import "os"

func fileOwner(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
