package router

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/router/middleware"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

const (
	betterFilesArchiveMaxEntries = 5000
	betterFilesArchiveMaxFile    = 512 * 1024 * 1024
	betterFilesArchiveMaxExtract = 2000
)

type betterFilesArchiveEntry struct {
	Name             string `json:"name"`
	Path             string `json:"path"`
	Directory        string `json:"directory"`
	IsDirectory      bool   `json:"is_directory"`
	CompressedSize   uint64 `json:"compressed_size"`
	UncompressedSize uint64 `json:"uncompressed_size"`
	ModifiedAt       string `json:"modified_at"`
}

type betterFilesArchiveExtractSelection struct {
	Path        string `json:"path"`
	IsDirectory bool   `json:"is_directory"`
}

type betterFilesArchiveExtractRequest struct {
	File        string                               `json:"file"`
	Destination string                               `json:"destination"`
	Entries     []betterFilesArchiveExtractSelection `json:"entries"`
}

func getServerArchiveList(c *gin.Context) {
	s := middleware.ExtractServer(c)
	file, displayPath, ok := betterFilesOpenServerRegularFile(s.Filesystem(), c.Query("file"))
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid archive path."})
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	if stat.IsDir() || stat.Size() > betterFilesArchiveMaxFile {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Archive is too large or is not a file."})
		return
	}
	if !betterFilesIsArchiveFile(displayPath) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Only zip, jar, war, tar, tar.gz, and tgz archives can be viewed inline."})
		return
	}

	entries, totalEntries, err := betterFilesListArchiveEntries(file, displayPath)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDirectory != entries[j].IsDirectory {
			return entries[i].IsDirectory
		}
		return strings.ToLower(entries[i].Path) < strings.ToLower(entries[j].Path)
	})

	c.JSON(http.StatusOK, gin.H{
		"file":          displayPath,
		"entries":       entries,
		"truncated":     totalEntries > betterFilesArchiveMaxEntries,
		"total_entries": totalEntries,
	})
}

func postServerArchiveExtract(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var request betterFilesArchiveExtractRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid archive extraction request."})
		return
	}

	if len(request.Entries) == 0 || len(request.Entries) > betterFilesArchiveMaxExtract {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Select between 1 and 2000 archive entries to extract."})
		return
	}

	file, displayPath, ok := betterFilesOpenServerRegularFile(s.Filesystem(), request.File)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid archive path."})
		return
	}
	defer file.Close()
	destinationDisplay, ok := betterFilesResolveServerDirectory(s.Filesystem(), request.Destination, true)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid extraction destination."})
		return
	}

	stat, err := file.Stat()
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	if stat.IsDir() || stat.Size() > betterFilesArchiveMaxFile || !betterFilesIsArchiveFile(displayPath) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Archive is too large or is not supported."})
		return
	}

	selections, err := betterFilesCleanArchiveSelections(request.Entries)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	count, err := betterFilesExtractArchiveEntries(s.Filesystem(), file, displayPath, destinationDisplay, selections)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"file":        displayPath,
		"destination": destinationDisplay,
		"extracted":   count,
	})
}

func betterFilesCleanServerPath(raw string, allowRoot bool) (string, bool) {
	cleaned := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if cleaned == "" || strings.Contains(cleaned, "\x00") || !utf8.ValidString(cleaned) || len(cleaned) > 4096 {
		return "", false
	}
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return "", false
		}
	}

	displayPath := path.Clean(cleaned)
	if displayPath == "." || (!allowRoot && displayPath == "/") || !strings.HasPrefix(displayPath, "/") {
		return "", false
	}

	return displayPath, true
}

func betterFilesOpenServerRegularFile(fs *serverfs.Filesystem, raw string) (ufs.File, string, bool) {
	displayPath, ok := betterFilesCleanServerPath(raw, false)
	if !ok {
		return nil, "", false
	}

	file, stat, err := fs.File(displayPath)
	if err != nil {
		return nil, "", false
	}
	if stat.IsDir() || !stat.Mode().IsRegular() {
		file.Close()
		return nil, "", false
	}
	return file, displayPath, true
}

func betterFilesResolveServerDirectory(fs *serverfs.Filesystem, raw string, mustExist bool) (string, bool) {
	displayPath, ok := betterFilesCleanServerPath(raw, true)
	if !ok {
		return "", false
	}

	stat, err := fs.Stat(displayPath)
	if err != nil {
		return displayPath, !mustExist
	}
	if !stat.IsDir() {
		return "", false
	}

	return displayPath, true
}

func betterFilesIsArchiveFile(value string) bool {
	return betterFilesIsZipFile(value) || betterFilesIsTarFile(value)
}

func betterFilesIsZipFile(value string) bool {
	ext := strings.ToLower(path.Ext(value))
	return ext == ".zip" || ext == ".jar" || ext == ".war"
}

func betterFilesIsTarFile(value string) bool {
	lower := strings.ToLower(value)
	return strings.HasSuffix(lower, ".tar") || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz")
}

func betterFilesListArchiveEntries(file ufs.File, displayPath string) ([]betterFilesArchiveEntry, int, error) {
	if betterFilesIsZipFile(displayPath) {
		return betterFilesListZipEntries(file)
	}
	if betterFilesIsTarFile(displayPath) {
		return betterFilesListTarEntries(file, displayPath)
	}
	return nil, 0, errors.New("unsupported archive type")
}

func betterFilesListZipEntries(file ufs.File) ([]betterFilesArchiveEntry, int, error) {
	stat, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	reader, err := zip.NewReader(file, stat.Size())
	if err != nil {
		return nil, 0, errors.New("could not open zip archive")
	}

	entryCapacity := len(reader.File)
	if entryCapacity > betterFilesArchiveMaxEntries {
		entryCapacity = betterFilesArchiveMaxEntries
	}
	entries := make([]betterFilesArchiveEntry, 0, entryCapacity)
	for idx, entry := range reader.File {
		if idx >= betterFilesArchiveMaxEntries {
			break
		}
		normalized, ok := betterFilesCleanArchiveEntryPath(entry.Name)
		if !ok {
			continue
		}

		entries = append(entries, betterFilesArchiveEntry{
			Name:             path.Base(normalized),
			Path:             "/" + normalized,
			Directory:        betterFilesArchiveEntryDirectory(normalized),
			IsDirectory:      entry.FileInfo().IsDir(),
			CompressedSize:   uint64(entry.CompressedSize64),
			UncompressedSize: uint64(entry.UncompressedSize64),
			ModifiedAt:       entry.Modified.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	return entries, len(reader.File), nil
}

func betterFilesListTarEntries(file ufs.File, displayPath string) ([]betterFilesArchiveEntry, int, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	var archiveReader io.Reader = file
	if strings.HasSuffix(strings.ToLower(displayPath), ".tar.gz") || strings.HasSuffix(strings.ToLower(displayPath), ".tgz") {
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			return nil, 0, errors.New("could not open gzip archive")
		}
		defer gzipReader.Close()
		archiveReader = gzipReader
	}

	tarReader := tar.NewReader(archiveReader)
	entries := make([]betterFilesArchiveEntry, 0, 128)
	totalEntries := 0
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, totalEntries, errors.New("could not read tar archive")
		}
		totalEntries++
		if totalEntries > betterFilesArchiveMaxEntries {
			continue
		}

		normalized, ok := betterFilesCleanArchiveEntryPath(header.Name)
		if !ok {
			continue
		}
		isDirectory := header.FileInfo().IsDir() || header.Typeflag == tar.TypeDir
		uncompressedSize := uint64(0)
		if header.Size > 0 {
			uncompressedSize = uint64(header.Size)
		}

		entries = append(entries, betterFilesArchiveEntry{
			Name:             path.Base(normalized),
			Path:             "/" + normalized,
			Directory:        betterFilesArchiveEntryDirectory(normalized),
			IsDirectory:      isDirectory,
			CompressedSize:   0,
			UncompressedSize: uncompressedSize,
			ModifiedAt:       header.ModTime.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	return entries, totalEntries, nil
}

func betterFilesCleanArchiveEntryPath(value string) (string, bool) {
	entryPath := path.Clean(strings.ReplaceAll(value, "\\", "/"))
	entryPath = strings.TrimPrefix(entryPath, "/")
	if entryPath == "." || entryPath == "" || strings.HasPrefix(entryPath, "../") || strings.Contains(entryPath, "/../") {
		return "", false
	}
	return entryPath, true
}

func betterFilesArchiveEntryDirectory(entryPath string) string {
	directory := path.Dir(entryPath)
	if directory == "." {
		return "/"
	}
	return "/" + directory
}

func betterFilesCleanArchiveSelections(entries []betterFilesArchiveExtractSelection) ([]betterFilesArchiveExtractSelection, error) {
	selections := make([]betterFilesArchiveExtractSelection, 0, len(entries))
	seen := map[string]struct{}{}
	for _, entry := range entries {
		cleaned, ok := betterFilesCleanArchiveEntryPath(entry.Path)
		if !ok {
			return nil, errors.New("Archive selection contains an invalid path.")
		}
		key := cleaned
		if entry.IsDirectory {
			key += "/"
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		selections = append(selections, betterFilesArchiveExtractSelection{
			Path:        cleaned,
			IsDirectory: entry.IsDirectory,
		})
	}
	return selections, nil
}

func betterFilesArchiveSelectionMatches(entryPath string, selections []betterFilesArchiveExtractSelection) bool {
	for _, selection := range selections {
		if selection.IsDirectory {
			if entryPath == selection.Path || strings.HasPrefix(entryPath, selection.Path+"/") {
				return true
			}
			continue
		}
		if entryPath == selection.Path {
			return true
		}
	}
	return false
}

type betterFilesArchiveWritableFilesystem interface {
	Write(string, io.Reader, int64, os.FileMode) error
	CreateDirectory(string, string) error
}

func betterFilesExtractArchiveEntries(fs betterFilesArchiveWritableFilesystem, file ufs.File, displayPath string, destination string, selections []betterFilesArchiveExtractSelection) (int, error) {
	if betterFilesIsZipFile(displayPath) {
		return betterFilesExtractZipEntries(fs, file, destination, selections)
	}
	if betterFilesIsTarFile(displayPath) {
		return betterFilesExtractTarEntries(fs, file, displayPath, destination, selections)
	}
	return 0, errors.New("unsupported archive type")
}

func betterFilesExtractOutputPath(destination string, entryPath string) string {
	return path.Clean(path.Join(destination, entryPath))
}

func betterFilesExtractZipEntries(fs betterFilesArchiveWritableFilesystem, file ufs.File, destination string, selections []betterFilesArchiveExtractSelection) (int, error) {
	stat, err := file.Stat()
	if err != nil {
		return 0, err
	}
	reader, err := zip.NewReader(file, stat.Size())
	if err != nil {
		return 0, errors.New("could not open zip archive")
	}

	extracted := 0
	for _, entry := range reader.File {
		normalized, ok := betterFilesCleanArchiveEntryPath(entry.Name)
		if !ok || !betterFilesArchiveSelectionMatches(normalized, selections) {
			continue
		}
		outputPath := betterFilesExtractOutputPath(destination, normalized)
		if entry.FileInfo().IsDir() {
			if err := fs.CreateDirectory(path.Base(outputPath), path.Dir(outputPath)); err != nil {
				return extracted, err
			}
			continue
		}

		file, err := entry.Open()
		if err != nil {
			return extracted, err
		}
		err = fs.Write(outputPath, file, int64(entry.UncompressedSize64), entry.FileInfo().Mode().Perm())
		file.Close()
		if err != nil {
			return extracted, err
		}
		extracted++
		if extracted > betterFilesArchiveMaxExtract {
			return extracted, errors.New("Too many files matched this extraction request.")
		}
	}

	if extracted == 0 {
		return 0, errors.New("No matching archive entries were found.")
	}
	return extracted, nil
}

func betterFilesExtractTarEntries(fs betterFilesArchiveWritableFilesystem, file ufs.File, displayPath string, destination string, selections []betterFilesArchiveExtractSelection) (int, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	var archiveReader io.Reader = file
	if strings.HasSuffix(strings.ToLower(displayPath), ".tar.gz") || strings.HasSuffix(strings.ToLower(displayPath), ".tgz") {
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			return 0, errors.New("could not open gzip archive")
		}
		defer gzipReader.Close()
		archiveReader = gzipReader
	}

	tarReader := tar.NewReader(archiveReader)
	extracted := 0
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return extracted, errors.New("could not read tar archive")
		}

		normalized, ok := betterFilesCleanArchiveEntryPath(header.Name)
		if !ok || !betterFilesArchiveSelectionMatches(normalized, selections) {
			continue
		}
		outputPath := betterFilesExtractOutputPath(destination, normalized)
		if header.FileInfo().IsDir() || header.Typeflag == tar.TypeDir {
			if err := fs.CreateDirectory(path.Base(outputPath), path.Dir(outputPath)); err != nil {
				return extracted, err
			}
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if err := fs.Write(outputPath, tarReader, header.Size, header.FileInfo().Mode().Perm()); err != nil {
			return extracted, err
		}
		extracted++
		if extracted > betterFilesArchiveMaxExtract {
			return extracted, errors.New("Too many files matched this extraction request.")
		}
	}

	if extracted == 0 {
		return 0, errors.New("No matching archive entries were found.")
	}
	return extracted, nil
}
