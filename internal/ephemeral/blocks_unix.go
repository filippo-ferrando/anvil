//go:build unix

package ephemeral

import (
	"io/fs"
	"syscall"
)

func blocks(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return info.Size()
}
