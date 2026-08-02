package filesystem

import (
	"errors"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/pterodactyl/wings/internal/ufs"
)

// RenameNoReplace atomically moves a regular file or directory while refusing
// to replace any destination that appears concurrently. This closes the
// lstat-then-rename race in bulk rename requests.
func (fs *Filesystem) RenameNoReplace(oldpath, newpath string) error {
	if oldpath == newpath {
		return nil
	}

	olddirfd, oldname, closeOld, err := fs.unixFS.SafePath(oldpath)
	defer closeOld()
	if err != nil {
		return err
	}
	if oldname == "." {
		return ufs.ErrBadPathResolution
	}
	oldInfo, err := fs.unixFS.Lstatat(olddirfd, oldname)
	if err != nil {
		return err
	}
	if oldInfo.Mode()&ufs.ModeSymlink != 0 || (!oldInfo.IsDir() && !oldInfo.Mode().IsRegular()) {
		return ufs.ErrBadPathResolution
	}

	newdirfd, newname, closeNew, err := fs.unixFS.SafePath(newpath)
	if err != nil {
		closeNew()
		if !errors.Is(err, ufs.ErrNotExist) {
			return err
		}
		if err := fs.mkdirAll(filepath.Dir(newpath), 0o755); err != nil {
			return err
		}
		newdirfd, newname, closeNew, err = fs.unixFS.SafePath(newpath)
		if err != nil {
			closeNew()
			return err
		}
	}
	defer closeNew()
	if newname == "." {
		return ufs.ErrBadPathResolution
	}

	for {
		err = unix.Renameat2(olddirfd, oldname, newdirfd, newname, unix.RENAME_NOREPLACE)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return &ufs.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
		}
		return nil
	}
}

// Replace atomically renames a regular temporary file over a regular target.
// It is used by streamed copies so a cancelled or failed copy never exposes a
// partially written destination. Quota usage for the replaced inode is removed
// only after the atomic rename succeeds.
func (fs *Filesystem) Replace(oldpath, newpath string) error {
	olddirfd, oldname, closeOld, err := fs.unixFS.SafePath(oldpath)
	defer closeOld()
	if err != nil {
		return err
	}
	if oldname == "." {
		return ufs.ErrBadPathResolution
	}
	oldInfo, err := fs.unixFS.Lstatat(olddirfd, oldname)
	if err != nil {
		return err
	}
	if !oldInfo.Mode().IsRegular() {
		return ufs.ErrBadPathResolution
	}

	newdirfd, newname, closeNew, err := fs.unixFS.SafePath(newpath)
	defer closeNew()
	if err != nil {
		return err
	}
	if newname == "." {
		return ufs.ErrBadPathResolution
	}

	var replacedSize int64
	newInfo, statErr := fs.unixFS.Lstatat(newdirfd, newname)
	switch {
	case statErr == nil:
		if !newInfo.Mode().IsRegular() {
			return ufs.ErrBadPathResolution
		}
		replacedSize = newInfo.Size()
	case !errors.Is(statErr, ufs.ErrNotExist):
		return statErr
	}

	for {
		err = unix.Renameat(olddirfd, oldname, newdirfd, newname)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return &ufs.LinkError{Op: "replace", Old: oldpath, New: newpath, Err: err}
		}
		break
	}
	if replacedSize > 0 {
		fs.adjustDisk(-replacedSize)
	}
	return nil
}
