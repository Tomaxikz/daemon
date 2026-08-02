package router

import (
	"time"

	"github.com/gabriel-vasile/mimetype"
	"golang.org/x/sys/unix"

	"github.com/pterodactyl/wings/internal/ufs"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

type betterFilesEntry struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	SizePhysical int64  `json:"size_physical"`
	Directory    bool   `json:"directory"`
	File         bool   `json:"file"`
	Symlink      bool   `json:"symlink"`
	Mime         string `json:"mime"`
	Created      string `json:"created"`
	Modified     string `json:"modified"`
}

func newBetterFilesEntry(name string, info ufs.FileInfo, mime string) betterFilesEntry {
	if mime == "" {
		if info.IsDir() {
			mime = "inode/directory"
		} else {
			mime = "application/octet-stream"
		}
	}
	return betterFilesEntry{
		Name:         name,
		Size:         info.Size(),
		SizePhysical: betterFilesPhysicalSize(info),
		Directory:    info.IsDir(),
		File:         info.Mode().IsRegular(),
		Symlink:      info.Mode()&ufs.ModeSymlink != 0,
		Mime:         mime,
		Created:      betterFilesCreatedTime(info).UTC().Format(time.RFC3339),
		Modified:     info.ModTime().UTC().Format(time.RFC3339),
	}
}

func betterFilesCreatedTime(info ufs.FileInfo) time.Time {
	if stat, ok := info.Sys().(*unix.Stat_t); ok {
		return time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec)
	}
	return info.ModTime()
}

func betterFilesMime(fs *serverfs.Filesystem, value string, info ufs.FileInfo) string {
	if info.IsDir() {
		return "inode/directory"
	}
	if !info.Mode().IsRegular() {
		return "application/octet-stream"
	}
	file, err := fs.UnixFS().OpenFile(value, ufs.O_RDONLY|ufs.O_NOFOLLOW, 0)
	if err != nil {
		return "application/octet-stream"
	}
	defer file.Close()
	detected, err := mimetype.DetectReader(file)
	if err != nil || detected == nil {
		return "application/octet-stream"
	}
	return detected.String()
}
