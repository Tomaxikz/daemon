package filesystem

import (
	"errors"
	"io"
	"io/fs"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/ufs"
)

// WriteUpload writes a regular upload through pinned directory descriptors.
// Every component is opened separately with O_NOFOLLOW, including on kernels
// without openat2. Quota growth is reserved before truncating the destination.
func (f *Filesystem) WriteUpload(name string, source io.Reader, size int64) (err error) {
	name = strings.TrimPrefix(name, "/")
	if size < 0 || !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\\x00") {
		return ufs.ErrBadPathResolution
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		if err := f.IsIgnored(prefix, "/"+prefix); err != nil {
			return err
		}
	}
	if err := f.HasSpaceFor(0); err != nil {
		return err
	}
	parent, err := f.unixFS.OpenFile(".", ufs.O_DIRECTORY|ufs.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	uid, gid := config.Get().System.User.Uid, config.Get().System.User.Gid
	for _, component := range parts[:len(parts)-1] {
		child, openErr := f.unixFS.OpenFileat(int(parent.Fd()), component, ufs.O_DIRECTORY|ufs.O_RDONLY|ufs.O_NOFOLLOW, 0)
		if errors.Is(openErr, ufs.ErrNotExist) {
			created := false
			if mkdirErr := f.unixFS.Mkdirat(int(parent.Fd()), component, 0o755); mkdirErr == nil {
				created = true
			} else if !errors.Is(mkdirErr, ufs.ErrExist) {
				return mkdirErr
			}
			child, openErr = f.unixFS.OpenFileat(int(parent.Fd()), component, ufs.O_DIRECTORY|ufs.O_RDONLY|ufs.O_NOFOLLOW, 0)
			if openErr == nil && created && !f.isTest {
				if err := unix.Fchown(int(child.Fd()), uid, gid); err != nil {
					_ = child.Close()
					return err
				}
			}
		}
		if openErr != nil {
			return openErr
		}
		_ = parent.Close()
		parent = child
	}
	// O_NONBLOCK avoids blocking on a raced-in FIFO. Do not truncate until the
	// opened inode is verified as regular and its quota growth is reserved.
	file, err := f.unixFS.OpenFileat(int(parent.Fd()), parts[len(parts)-1], ufs.O_CREATE|ufs.O_RDWR|ufs.O_NOFOLLOW|unix.O_NONBLOCK, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if !info.Mode().IsRegular() || stat.Nlink != 1 {
		return ufs.ErrBadPathResolution
	}
	growth := size - info.Size()
	if err := f.HasSpaceFor(growth); err != nil {
		return err
	}
	reserved := int64(0)
	if growth > 0 {
		if err := f.reserveDisk(growth); err != nil {
			return err
		}
		reserved = growth
	}
	defer func() {
		if after, statErr := file.Stat(); statErr == nil {
			f.adjustDisk(after.Size() - info.Size() - reserved)
		} else if err == nil {
			err = statErr
		}
	}()
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := io.CopyN(file, source, size); err != nil {
		return err
	}
	if !f.isTest {
		return unix.Fchown(int(file.Fd()), uid, gid)
	}
	return nil
}
