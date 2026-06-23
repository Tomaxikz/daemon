package filesystem

import (
	"emperror.dev/errors"
)

// Checks if the given file or path is in the server's file denylist. If so, an Error
// is returned, otherwise nil is returned.
func (fs *Filesystem) IsIgnored(paths ...string) error {
	for _, p := range paths {
		//sp, err := fs.SafePath(p)
		//if err != nil {
		//	return err
		//}
		// TODO: update logic to use unixFS
		if fs.denylist.MatchesPath(p) {
			return errors.WithStack(&Error{code: ErrCodeDenylistFile, path: p, resolved: p})
		}
	}
	return nil
}
