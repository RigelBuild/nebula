//go:build darwin || linux

package transfer

import "golang.org/x/sys/unix"

type fileStat = unix.Stat_t
