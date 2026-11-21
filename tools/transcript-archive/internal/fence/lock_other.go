//go:build !linux && !darwin

package fence

import "os"

// changeTime reports 0 where this package does not read the change time, so
// sameLockFile compares size and mtime alone, as it did before ctime was checked.
func changeTime(_ os.FileInfo) int64 { return 0 }
