package main

import (
	"errors"
	"fmt"
	"path/filepath"
)

// fsType values reported by the filesystem seam.
const fsBtrfs = "btrfs"

// errOperationNotSupported is what chattr +C returns on filesystems without
// NOCOW, such as ext4. Only this error is ignored; any other failure stops.
var errOperationNotSupported = errors.New("operation not supported")

// imageFS is the filesystem seam. The real implementation uses the kernel;
// tests substitute a fake so every branch runs without btrfs.
type imageFS interface {
	// Exists reports whether path exists.
	Exists(path string) (bool, error)
	// FilesystemType returns the filesystem type name of path's mount.
	FilesystemType(path string) (string, error)
	// NoCOW reports whether path has the NOCOW attribute set.
	NoCOW(path string) (bool, error)
	// CreateEmpty removes path and creates an empty file at it.
	CreateEmpty(path string) error
	// SetNoCOW sets the NOCOW attribute on an empty file. It returns
	// errOperationNotSupported when the filesystem cannot do NOCOW.
	SetNoCOW(path string) error
	// Truncate sets the apparent size of path to size bytes. It allocates no
	// blocks, so the image takes disk space only as the filesystem writes.
	Truncate(path string, size int64) error
	// Mkfs formats path as ext4.
	Mkfs(path string) error
	// Rename moves from to to, replacing to.
	Rename(from, to string) error
	// Sync flushes path to stable storage. It works on a regular file or a
	// directory, so the rename can be made durable with the same call.
	Sync(path string) error
	// Mounted reports whether mount is an active mount point.
	Mounted(mount string) (bool, error)
}

// config is one image preparation request.
type config struct {
	image     string
	temporary string
	mount     string
	size      int64
}

// prepare creates or migrates the gate-run image. It rebuilds an image only
// when the filesystem can hold NOCOW and the attribute is missing; a failed
// read is an error, never a rebuild, so caches are never destroyed by a
// misread. An existing image is never resized or preallocated here.
func prepare(fsys imageFS, cfg config, log func(string)) error {
	exists, err := fsys.Exists(cfg.image)
	if err != nil {
		return fmt.Errorf("stat %s: %w", cfg.image, err)
	}
	if !exists {
		return build(fsys, cfg)
	}
	return migrate(fsys, cfg, log)
}

// migrate rebuilds an existing image once, and only on btrfs without NOCOW.
// On ext4 the attribute can never be set, so rebuilding would destroy the
// caches on every boot and is skipped.
func migrate(fsys imageFS, cfg config, log func(string)) error {
	kind, err := fsys.FilesystemType(cfg.image)
	if err != nil {
		return fmt.Errorf("filesystem type of %s: %w", cfg.image, err)
	}
	if kind != fsBtrfs {
		return nil
	}
	nocow, err := fsys.NoCOW(cfg.image)
	if err != nil {
		return fmt.Errorf("read attributes of %s: %w", cfg.image, err)
	}
	if nocow {
		return nil
	}
	mounted, err := fsys.Mounted(cfg.mount)
	if err != nil {
		return fmt.Errorf("check mount %s: %w", cfg.mount, err)
	}
	if mounted {
		log(cfg.image + " lacks the NOCOW attribute but is mounted; rebuild it on the next boot")
		return nil
	}
	return build(fsys, cfg)
}

// build creates a sparse NOCOW image in the temporary path, then renames it
// into place. chattr +C works only on an empty file, so it runs before truncate.
func build(fsys imageFS, cfg config) error {
	if err := fsys.CreateEmpty(cfg.temporary); err != nil {
		return fmt.Errorf("create %s: %w", cfg.temporary, err)
	}
	if err := fsys.SetNoCOW(cfg.temporary); err != nil && !errors.Is(err, errOperationNotSupported) {
		return fmt.Errorf("chattr +C %s: %w", cfg.temporary, err)
	}
	if err := fsys.Truncate(cfg.temporary, cfg.size); err != nil {
		return fmt.Errorf("truncate %s: %w", cfg.temporary, err)
	}
	if err := fsys.Mkfs(cfg.temporary); err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w", cfg.temporary, err)
	}
	// Flush the formatted image before it becomes the live name, so a crash
	// after the rename cannot leave a partly written ext4 at the image path.
	if err := fsys.Sync(cfg.temporary); err != nil {
		return fmt.Errorf("fsync %s: %w", cfg.temporary, err)
	}
	if err := fsys.Rename(cfg.temporary, cfg.image); err != nil {
		return fmt.Errorf("rename %s: %w", cfg.temporary, err)
	}
	// The rename is a directory entry change; sync the parent to persist it.
	if err := fsys.Sync(filepath.Dir(cfg.image)); err != nil {
		return fmt.Errorf("fsync %s: %w", filepath.Dir(cfg.image), err)
	}
	return nil
}
