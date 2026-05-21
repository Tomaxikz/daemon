package router

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Tnze/go-mc/nbt"
	"github.com/Tnze/go-mc/nbt/dynbt"
	"github.com/gin-gonic/gin"
	"github.com/pterodactyl/wings/router/middleware"
)

const (
	betterFilesArchiveMaxEntries = 5000
	betterFilesArchiveMaxFile    = 512 * 1024 * 1024
	betterFilesArchiveMaxExtract = 2000
	betterFilesNbtMaxFile        = 16 * 1024 * 1024
	betterFilesNbtMaxJson        = 24 * 1024 * 1024
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
	File        string                                `json:"file"`
	Destination string                                `json:"destination"`
	Entries     []betterFilesArchiveExtractSelection `json:"entries"`
}

type betterFilesNbtDocument struct {
	Compression string              `json:"compression"`
	Root        betterFilesNbtNamed `json:"root"`
}

type betterFilesNbtNamed struct {
	Name  string             `json:"name"`
	Type  string             `json:"type"`
	Value betterFilesNbtNode `json:"value"`
}

type betterFilesNbtNode struct {
	Type        string                         `json:"type"`
	ElementType string                         `json:"element_type,omitempty"`
	Value       any                            `json:"value,omitempty"`
	Children    map[string]betterFilesNbtNode `json:"children,omitempty"`
	Items       []betterFilesNbtNode          `json:"items,omitempty"`
}

type betterFilesNbtWriteRequest struct {
	File     string                 `json:"file"`
	Document betterFilesNbtDocument `json:"document"`
}

func getServerArchiveList(c *gin.Context) {
	s := middleware.ExtractServer(c)
	hostPath, displayPath, ok := betterFilesResolveServerFile(s.Filesystem().Path(), c.Query("file"), true)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid archive path."})
		return
	}

	stat, err := os.Stat(hostPath)
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

	entries, totalEntries, err := betterFilesListArchiveEntries(hostPath, displayPath)
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
		"file": displayPath,
		"entries": entries,
		"truncated": totalEntries > betterFilesArchiveMaxEntries,
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

	root := s.Filesystem().Path()
	hostPath, displayPath, ok := betterFilesResolveServerFile(root, request.File, true)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid archive path."})
		return
	}
	destinationDisplay, ok := betterFilesResolveServerDirectory(root, request.Destination, true)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid extraction destination."})
		return
	}

	stat, err := os.Stat(hostPath)
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

	count, err := betterFilesExtractArchiveEntries(s.Filesystem(), hostPath, displayPath, destinationDisplay, selections)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"file": displayPath,
		"destination": destinationDisplay,
		"extracted": count,
	})
}

func getServerNbtFile(c *gin.Context) {
	s := middleware.ExtractServer(c)
	hostPath, displayPath, ok := betterFilesResolveServerFile(s.Filesystem().Path(), c.Query("file"), true)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid NBT path."})
		return
	}

	document, err := betterFilesReadNbtDocument(hostPath)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"file": displayPath,
		"document": document,
	})
}

func putServerNbtFile(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var request betterFilesNbtWriteRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid NBT request."})
		return
	}

	hostPath, displayPath, ok := betterFilesResolveServerFile(s.Filesystem().Path(), request.File, true)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid NBT path."})
		return
	}
	if !betterFilesIsNbtFile(displayPath) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Only .nbt and .dat files can be edited as NBT."})
		return
	}

	payload, err := json.Marshal(request.Document)
	if err != nil || len(payload) > betterFilesNbtMaxJson {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "NBT document is too large."})
		return
	}

	if err := betterFilesWriteNbtDocument(hostPath, request.Document); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"file": displayPath})
}

func betterFilesResolveServerFile(root string, raw string, mustExist bool) (string, string, bool) {
	cleaned := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if cleaned == "" || strings.Contains(cleaned, "\x00") || !utf8.ValidString(cleaned) || len(cleaned) > 4096 {
		return "", "", false
	}
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return "", "", false
		}
	}

	displayPath := path.Clean(cleaned)
	if displayPath == "." || displayPath == "/" || !strings.HasPrefix(displayPath, "/") {
		return "", "", false
	}

	rootPath, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", "", false
	}
	hostPath := filepath.Join(rootPath, strings.TrimPrefix(displayPath, "/"))
	hostPath, err = filepath.Abs(hostPath)
	if err != nil {
		return "", "", false
	}
	rel, err := filepath.Rel(rootPath, hostPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", "", false
	}

	stat, err := os.Lstat(hostPath)
	if err != nil {
		if mustExist {
			return "", "", false
		}
		return hostPath, displayPath, true
	}
	if stat.Mode()&os.ModeSymlink != 0 || !stat.Mode().IsRegular() {
		return "", "", false
	}

	return hostPath, displayPath, true
}

func betterFilesResolveServerDirectory(root string, raw string, mustExist bool) (string, bool) {
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
	if displayPath == "." || !strings.HasPrefix(displayPath, "/") {
		return "", false
	}

	rootPath, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", false
	}
	hostPath := filepath.Join(rootPath, strings.TrimPrefix(displayPath, "/"))
	hostPath, err = filepath.Abs(hostPath)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(rootPath, hostPath)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", false
	}

	stat, err := os.Lstat(hostPath)
	if err != nil {
		return displayPath, !mustExist
	}
	if stat.Mode()&os.ModeSymlink != 0 || !stat.IsDir() {
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

func betterFilesIsNbtFile(value string) bool {
	ext := strings.ToLower(path.Ext(value))
	return ext == ".nbt" || ext == ".dat"
}

func betterFilesListArchiveEntries(hostPath string, displayPath string) ([]betterFilesArchiveEntry, int, error) {
	if betterFilesIsZipFile(displayPath) {
		return betterFilesListZipEntries(hostPath)
	}
	if betterFilesIsTarFile(displayPath) {
		return betterFilesListTarEntries(hostPath, displayPath)
	}
	return nil, 0, errors.New("unsupported archive type")
}

func betterFilesListZipEntries(hostPath string) ([]betterFilesArchiveEntry, int, error) {
	reader, err := zip.OpenReader(hostPath)
	if err != nil {
		return nil, 0, errors.New("could not open zip archive")
	}
	defer reader.Close()

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

func betterFilesListTarEntries(hostPath string, displayPath string) ([]betterFilesArchiveEntry, int, error) {
	file, err := os.Open(hostPath)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

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

func betterFilesExtractArchiveEntries(fs betterFilesArchiveWritableFilesystem, hostPath string, displayPath string, destination string, selections []betterFilesArchiveExtractSelection) (int, error) {
	if betterFilesIsZipFile(displayPath) {
		return betterFilesExtractZipEntries(fs, hostPath, destination, selections)
	}
	if betterFilesIsTarFile(displayPath) {
		return betterFilesExtractTarEntries(fs, hostPath, displayPath, destination, selections)
	}
	return 0, errors.New("unsupported archive type")
}

func betterFilesExtractOutputPath(destination string, entryPath string) string {
	return path.Clean(path.Join(destination, entryPath))
}

func betterFilesExtractZipEntries(fs betterFilesArchiveWritableFilesystem, hostPath string, destination string, selections []betterFilesArchiveExtractSelection) (int, error) {
	reader, err := zip.OpenReader(hostPath)
	if err != nil {
		return 0, errors.New("could not open zip archive")
	}
	defer reader.Close()

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

func betterFilesExtractTarEntries(fs betterFilesArchiveWritableFilesystem, hostPath string, displayPath string, destination string, selections []betterFilesArchiveExtractSelection) (int, error) {
	file, err := os.Open(hostPath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

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

func betterFilesReadNbtDocument(hostPath string) (betterFilesNbtDocument, error) {
	stat, err := os.Stat(hostPath)
	if err != nil {
		return betterFilesNbtDocument{}, err
	}
	if stat.IsDir() || stat.Size() <= 0 || stat.Size() > betterFilesNbtMaxFile {
		return betterFilesNbtDocument{}, errors.New("NBT file is empty, too large, or not a regular file")
	}

	raw, err := os.ReadFile(hostPath)
	if err != nil {
		return betterFilesNbtDocument{}, err
	}

	data, compression, err := betterFilesDecodeNbtCompression(raw)
	if err != nil {
		return betterFilesNbtDocument{}, err
	}

	var root dynbt.Value
	rootName, err := nbt.NewDecoder(bytes.NewReader(data)).Decode(&root)
	if err != nil {
		return betterFilesNbtDocument{}, fmt.Errorf("could not decode NBT document: %w", err)
	}
	if root.TagType() != nbt.TagCompound {
		return betterFilesNbtDocument{}, errors.New("NBT root tag must be a compound")
	}

	return betterFilesNbtDocument{
		Compression: compression,
		Root: betterFilesNbtNamed{
			Name:  rootName,
			Type:  betterFilesNbtTypeName(root.TagType()),
			Value: betterFilesNbtNodeFromDynbt(&root),
		},
	}, nil
}

func betterFilesWriteNbtDocument(hostPath string, document betterFilesNbtDocument) error {
	if document.Root.Type != "compound" || document.Root.Value.Type != "compound" {
		return errors.New("NBT root must be a compound tag")
	}
	if !utf8.ValidString(document.Root.Name) || len(document.Root.Name) > 65535 {
		return errors.New("NBT root name is invalid or too large")
	}

	root, err := betterFilesDynbtFromNbtNode(document.Root.Value)
	if err != nil {
		return err
	}
	if root.TagType() != nbt.TagCompound {
		return errors.New("NBT root must be a compound tag")
	}

	var out bytes.Buffer
	if err := nbt.NewEncoder(&out).Encode(root, document.Root.Name); err != nil {
		return err
	}

	encoded, err := betterFilesEncodeNbtCompression(out.Bytes(), document.Compression)
	if err != nil {
		return err
	}
	if len(encoded) > betterFilesNbtMaxFile {
		return errors.New("encoded NBT file is too large")
	}

	mode := os.FileMode(0640)
	if stat, err := os.Stat(hostPath); err == nil {
		mode = stat.Mode().Perm()
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(hostPath), ".betterfiles-nbt-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(encoded); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Chmod(mode); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, hostPath)
}

func betterFilesDecodeNbtCompression(raw []byte) ([]byte, string, error) {
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, "", err
		}
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, betterFilesNbtMaxFile+1))
		if len(data) > betterFilesNbtMaxFile {
			return nil, "", errors.New("NBT payload is too large")
		}
		return data, "gzip", err
	}

	if len(raw) >= 2 && raw[0] == 0x78 {
		reader, err := zlib.NewReader(bytes.NewReader(raw))
		if err == nil {
			defer reader.Close()
			data, err := io.ReadAll(io.LimitReader(reader, betterFilesNbtMaxFile+1))
			if len(data) > betterFilesNbtMaxFile {
				return nil, "", errors.New("NBT payload is too large")
			}
			return data, "zlib", err
		}
	}

	return raw, "none", nil
}

func betterFilesEncodeNbtCompression(raw []byte, compression string) ([]byte, error) {
	var out bytes.Buffer
	switch compression {
	case "gzip", "":
		writer := gzip.NewWriter(&out)
		if _, err := writer.Write(raw); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	case "zlib":
		writer := zlib.NewWriter(&out)
		if _, err := writer.Write(raw); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	case "none":
		return raw, nil
	default:
		return nil, errors.New("unsupported NBT compression")
	}
}

func betterFilesNbtNodeFromDynbt(value *dynbt.Value) betterFilesNbtNode {
	if value == nil {
		return betterFilesNbtNode{Type: "end"}
	}

	switch value.TagType() {
	case nbt.TagByte:
		return betterFilesNbtNode{Type: "byte", Value: int(value.Byte())}
	case nbt.TagShort:
		return betterFilesNbtNode{Type: "short", Value: value.Short()}
	case nbt.TagInt:
		return betterFilesNbtNode{Type: "int", Value: value.Int()}
	case nbt.TagLong:
		return betterFilesNbtNode{Type: "long", Value: fmt.Sprintf("%d", value.Long())}
	case nbt.TagFloat:
		return betterFilesNbtNode{Type: "float", Value: value.Float()}
	case nbt.TagDouble:
		return betterFilesNbtNode{Type: "double", Value: value.Double()}
	case nbt.TagByteArray:
		raw := value.ByteArray()
		values := make([]int, 0, len(raw))
		for _, entry := range raw {
			values = append(values, int(int8(entry)))
		}
		return betterFilesNbtNode{Type: "byte_array", Value: values}
	case nbt.TagString:
		return betterFilesNbtNode{Type: "string", Value: value.String()}
	case nbt.TagList:
		rawItems := value.List()
		items := make([]betterFilesNbtNode, 0, len(rawItems))
		elementType := ""
		for _, item := range rawItems {
			if item != nil && elementType == "" {
				elementType = betterFilesNbtTypeName(item.TagType())
			}
			items = append(items, betterFilesNbtNodeFromDynbt(item))
		}
		return betterFilesNbtNode{Type: "list", ElementType: elementType, Items: items}
	case nbt.TagCompound:
		children := map[string]betterFilesNbtNode{}
		if compound := value.Compound(); compound != nil {
			compound.Visit(func(tag string, child *dynbt.Value) {
				children[tag] = betterFilesNbtNodeFromDynbt(child)
			})
		}
		return betterFilesNbtNode{Type: "compound", Children: children}
	case nbt.TagIntArray:
		return betterFilesNbtNode{Type: "int_array", Value: value.IntArray()}
	case nbt.TagLongArray:
		raw := value.LongArray()
		values := make([]string, 0, len(raw))
		for _, entry := range raw {
			values = append(values, fmt.Sprintf("%d", entry))
		}
		return betterFilesNbtNode{Type: "long_array", Value: values}
	default:
		return betterFilesNbtNode{Type: "end"}
	}
}

func betterFilesDynbtFromNbtNode(node betterFilesNbtNode) (*dynbt.Value, error) {
	switch node.Type {
	case "byte":
		return dynbt.NewByte(int8(numberFromAny(node.Value))), nil
	case "short":
		return dynbt.NewShort(int16(numberFromAny(node.Value))), nil
	case "int":
		return dynbt.NewInt(int32(numberFromAny(node.Value))), nil
	case "long":
		return dynbt.NewLong(numberFromAny(node.Value)), nil
	case "float":
		return dynbt.NewFloat(float32(floatFromAny(node.Value))), nil
	case "double":
		return dynbt.NewDouble(floatFromAny(node.Value)), nil
	case "byte_array":
		values := arrayFromAny(node.Value)
		raw := make([]byte, 0, len(values))
		for _, value := range values {
			raw = append(raw, byte(int8(value)))
		}
		return dynbt.NewByteArray(raw), nil
	case "string":
		value, _ := node.Value.(string)
		if !utf8.ValidString(value) || len(value) > 65535 {
			return nil, errors.New("NBT string is invalid or too large")
		}
		return dynbt.NewString(value), nil
	case "list":
		items := make([]*dynbt.Value, 0, len(node.Items))
		var elementType byte
		for _, item := range node.Items {
			value, err := betterFilesDynbtFromNbtNode(item)
			if err != nil {
				return nil, err
			}
			if elementType == 0 {
				elementType = value.TagType()
			} else if elementType != value.TagType() {
				return nil, errors.New("NBT list items must all use the same tag type")
			}
			items = append(items, value)
		}
		return dynbt.NewList(items...), nil
	case "compound":
		compound := dynbt.NewCompound()
		keys := make([]string, 0, len(node.Children))
		for key := range node.Children {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !utf8.ValidString(key) || len(key) > 65535 {
				return nil, errors.New("NBT compound key is invalid or too large")
			}
			child, err := betterFilesDynbtFromNbtNode(node.Children[key])
			if err != nil {
				return nil, err
			}
			compound.Set(key, child)
		}
		return compound, nil
	case "int_array":
		values := arrayFromAny(node.Value)
		raw := make([]int32, 0, len(values))
		for _, value := range values {
			raw = append(raw, int32(value))
		}
		return dynbt.NewIntArray(raw), nil
	case "long_array":
		return dynbt.NewLongArray(arrayFromAny(node.Value)), nil
	default:
		return nil, fmt.Errorf("unsupported NBT node type %s", node.Type)
	}
}

func betterFilesNbtTypeName(tagType byte) string {
	switch tagType {
	case 1:
		return "byte"
	case 2:
		return "short"
	case 3:
		return "int"
	case 4:
		return "long"
	case 5:
		return "float"
	case 6:
		return "double"
	case 7:
		return "byte_array"
	case 8:
		return "string"
	case 9:
		return "list"
	case 10:
		return "compound"
	case 11:
		return "int_array"
	case 12:
		return "long_array"
	default:
		return "end"
	}
}

func numberFromAny(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	case int32:
		return int64(typed)
	case string:
		var parsed int64
		_, _ = fmt.Sscanf(typed, "%d", &parsed)
		return parsed
	default:
		return 0
	}
}

func floatFromAny(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case string:
		var parsed float64
		_, _ = fmt.Sscanf(typed, "%f", &parsed)
		return parsed
	default:
		return 0
	}
}

func arrayFromAny(value any) []int64 {
	switch typed := value.(type) {
	case []any:
		values := make([]int64, 0, len(typed))
		for _, entry := range typed {
			values = append(values, numberFromAny(entry))
		}
		return values
	case []int:
		values := make([]int64, 0, len(typed))
		for _, entry := range typed {
			values = append(values, int64(entry))
		}
		return values
	case []int32:
		values := make([]int64, 0, len(typed))
		for _, entry := range typed {
			values = append(values, int64(entry))
		}
		return values
	case []int64:
		return typed
	case []float64:
		values := make([]int64, 0, len(typed))
		for _, entry := range typed {
			values = append(values, int64(entry))
		}
		return values
	case []string:
		values := make([]int64, 0, len(typed))
		for _, entry := range typed {
			values = append(values, numberFromAny(entry))
		}
		return values
	default:
		return nil
	}
}
