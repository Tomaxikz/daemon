package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apex/log"
	"github.com/pkg/sftp"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/secsy/goftp"
	"golang.org/x/crypto/ssh"
)

var (
	importStateMap   = make(map[string]*ImportProgress)
	importStateMutex sync.RWMutex
)

var ErrImportInProgress = errors.New("server import already running")

const importConnectionTimeout = 30 * time.Second
const (
	importProgressModeLive    = "live"
	importProgressModeScan    = "scan"
	importTransferIdleTimeout = 30 * time.Second
	importTerminalProgressTTL = 10 * time.Minute
)

type importReadResult struct {
	n   int
	err error
}

type importIdleReader struct {
	reader  io.Reader
	timeout time.Duration
	close   func() error
}

func newImportIdleReader(reader io.Reader, timeout time.Duration, closeFn func() error) io.Reader {
	if timeout <= 0 {
		return reader
	}
	return &importIdleReader{reader: reader, timeout: timeout, close: closeFn}
}

func (r *importIdleReader) Read(p []byte) (int, error) {
	result := make(chan importReadResult, 1)
	buffer := make([]byte, len(p))
	go func() {
		n, err := r.reader.Read(buffer)
		result <- importReadResult{n: n, err: err}
	}()

	timer := time.NewTimer(r.timeout)
	defer timer.Stop()

	select {
	case read := <-result:
		if read.n > 0 {
			copy(p, buffer[:read.n])
		}
		return read.n, read.err
	case <-timer.C:
		if r.close != nil {
			_ = r.close()
		}
		return 0, fmt.Errorf("import stalled: no data received for %s", r.timeout)
	}
}

type importIdleWriter struct {
	writer   io.Writer
	activity chan<- struct{}
}

func (w *importIdleWriter) Write(p []byte) (int, error) {
	select {
	case w.activity <- struct{}{}:
	default:
	}
	return w.writer.Write(p)
}

type ImportEstimate struct {
	Files int64
	Bytes int64
}

type SessionHostKeyStore struct {
	mu      sync.Mutex
	entries map[string]string
}

func NewSessionHostKeyStore() *SessionHostKeyStore {
	return &SessionHostKeyStore{
		entries: map[string]string{},
	}
}

func (s *SessionHostKeyStore) VerifyOrStore(hostKey string, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.entries[hostKey]; ok {
		if !strings.EqualFold(existing, fingerprint) {
			return fmt.Errorf("ssh host key mismatch (expected %s, got %s)", existing, fingerprint)
		}
		return nil
	}

	s.entries[hostKey] = fingerprint
	return nil
}

func getImportProgress(uuid string) *ImportProgress {
	importStateMutex.RLock()
	progress := importStateMap[uuid]
	importStateMutex.RUnlock()

	if progress != nil && progress.IsExpiredTerminal(importTerminalProgressTTL) {
		clearImportProgressIf(uuid, progress)
		return nil
	}

	return progress
}

func setImportProgress(uuid string, p *ImportProgress) {
	importStateMutex.Lock()
	defer importStateMutex.Unlock()
	if p == nil {
		delete(importStateMap, uuid)
	} else {
		importStateMap[uuid] = p
	}
}

func clearImportProgressIf(uuid string, p *ImportProgress) {
	importStateMutex.Lock()
	defer importStateMutex.Unlock()
	if importStateMap[uuid] == p {
		delete(importStateMap, uuid)
	}
}

func beginImportProgress(uuid string, mode string) bool {
	importStateMutex.Lock()
	defer importStateMutex.Unlock()
	if existing, ok := importStateMap[uuid]; ok && existing.IsImporting() {
		return false
	}
	if mode != importProgressModeScan {
		mode = importProgressModeLive
	}
	importStateMap[uuid] = &ImportProgress{
		Mode:           mode,
		Status:         "queued",
		TotalFiles:     -1,
		ProcessedFiles: 0,
		TotalBytes:     -1,
		ProcessedBytes: 0,
		Percentage:     0,
	}
	return true
}

func (s *Server) ImportNew(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, srcLocation, dstLocation, transferType, authMethod, progressMode string, wipe bool) error {
	if !beginImportProgress(s.ID(), progressMode) {
		return ErrImportInProgress
	}
	releaseProgress := true
	defer func() {
		if releaseProgress {
			setImportProgress(s.ID(), nil)
		}
	}()

	if err := s.ensureServerStopped(); err != nil {
		return err
	}

	if wipe {
		if err := s.Filesystem().TruncateRootDirectory(); err != nil {
			return err
		}
	}

	srcLocation, dstLocation = normalizePaths(srcLocation, dstLocation)
	releaseProgress = false
	return s.executeImport(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, srcLocation, dstLocation, transferType, authMethod, progressMode, nil)
}

func (s *Server) ImportNewSelected(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, srcLocation, dstLocation, transferType, authMethod, progressMode string, wipe bool, selectedItems []string) error {
	if !beginImportProgress(s.ID(), progressMode) {
		return ErrImportInProgress
	}
	releaseProgress := true
	defer func() {
		if releaseProgress {
			setImportProgress(s.ID(), nil)
		}
	}()

	if err := s.ensureServerStopped(); err != nil {
		return err
	}

	if wipe {
		if err := s.Filesystem().TruncateRootDirectory(); err != nil {
			return err
		}
	}

	srcLocation, dstLocation = normalizePaths(srcLocation, dstLocation)
	releaseProgress = false
	return s.executeImport(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, srcLocation, dstLocation, transferType, authMethod, progressMode, selectedItems)
}

func (s *Server) executeImport(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, srcLocation, dstLocation, transferType, authMethod, progressMode string, selectedItems []string) error {
	uuid := s.ID()
	var err error
	started := time.Now()
	sessionKeys := NewSessionHostKeyStore()

	defer func() {
		if progress := getImportProgress(uuid); progress != nil {
			if err != nil {
				progress.SetError(s, err.Error())
			} else {
				progress.Complete(s)
			}
		}

		if notifyErr := s.notifyImportComplete(err); notifyErr != nil {
			s.Log().WithField("error", notifyErr).Warn("failed to notify Panel of import completion, status may need manual clearing")
		} else {
			s.Log().Debug("notified Panel to clear import status")
		}

		s.Events().Publish(BackupRestoreCompletedEvent, "")
	}()

	s.Log().Info("syncing server state before import")
	if err := s.Sync(); err != nil {
		return err
	}

	isSelective := len(selectedItems) > 0
	isFTP := transferType == "ftp"
	if authMethod == "" {
		authMethod = "password"
	}
	if isFTP {
		authMethod = "password"
	}

	s.Log().WithFields(log.Fields{
		"type":        transferType,
		"auth_method": authMethod,
		"host":        host,
		"port":        port,
		"src":         srcLocation,
		"dst":         dstLocation,
		"progress":    progressMode,
		"selective":   isSelective,
		"items":       len(selectedItems),
	}).Info("starting import process")

	if progress := getImportProgress(uuid); progress != nil {
		progress.SetStatus(s, "scanning")
		if !progress.ShouldScan() {
			progress.SetStatus(s, "transferring")
		}
	}

	if isFTP {
		if isSelective {
			err = s.importSelectedFTP(user, password, host, port, srcLocation, dstLocation, selectedItems)
		} else {
			err = s.importFullFTP(user, password, host, port, srcLocation, dstLocation)
		}
	} else {
		useSshKey := authMethod == "ssh_key"
		sftpPassword := ""
		sftpKey := ""
		sftpKeyPassphrase := ""

		if useSshKey {
			sftpKey = sshKey
			sftpKeyPassphrase = sshKeyPassphrase
		} else {
			sftpPassword = password
		}

		if transferType == "scp" {
			if isSelective {
				err = s.importSelectedSCP(user, sftpPassword, sftpKey, sftpKeyPassphrase, hostKeyFingerprint, host, port, srcLocation, dstLocation, selectedItems, sessionKeys)
			} else {
				err = s.importFullSCP(user, sftpPassword, sftpKey, sftpKeyPassphrase, hostKeyFingerprint, host, port, srcLocation, dstLocation, sessionKeys)
			}
		} else {
			if isSelective {
				err = s.importSelectedSFTP(user, sftpPassword, sftpKey, sftpKeyPassphrase, hostKeyFingerprint, host, port, srcLocation, dstLocation, selectedItems, sessionKeys)
			} else {
				err = s.importFullSFTP(user, sftpPassword, sftpKey, sftpKeyPassphrase, hostKeyFingerprint, host, port, srcLocation, dstLocation, sessionKeys)
			}
		}
	}

	if err != nil {
		s.Log().WithFields(log.Fields{
			"error":       err,
			"type":        transferType,
			"auth_method": authMethod,
			"host":        host,
			"port":        port,
			"src":         srcLocation,
			"dst":         dstLocation,
			"progress":    progressMode,
			"selective":   isSelective,
			"items":       len(selectedItems),
			"duration":    time.Since(started),
		}).Error("import process failed")
	} else {
		s.Log().WithFields(log.Fields{
			"type":        transferType,
			"auth_method": authMethod,
			"host":        host,
			"port":        port,
			"src":         srcLocation,
			"dst":         dstLocation,
			"progress":    progressMode,
			"selective":   isSelective,
			"items":       len(selectedItems),
			"duration":    time.Since(started),
		}).Info("import process completed successfully")
	}

	return err
}

func (s *Server) importFullSFTP(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, srcLocation, dstLocation string, sessionKeys *SessionHostKeyStore) error {
	client, conn, err := s.connectSFTP(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, sessionKeys)
	if err != nil {
		return err
	}
	defer client.Close()
	defer conn.Close()

	srcLocation, dstLocation = normalizePathsForTransfer(srcLocation, dstLocation)
	remotePath := "/" + srcLocation

	info, err := client.Stat(remotePath)
	if err != nil {
		s.Log().WithField("error", err).Error("Unable to access remote path: " + remotePath)
		return err
	}

	if !info.IsDir() {
		if progress := getImportProgress(s.ID()); progress != nil {
			if progress.ShouldScan() {
				progress.SetTotals(s, 1, info.Size())
				progress.SetStatus(s, "transferring")
			}
		}
		return s.downloadSingleFileSFTP(client, remotePath, dstLocation)
	}

	if progress := getImportProgress(s.ID()); progress != nil {
		if progress.ShouldScan() {
			if estimate, err := countSFTPFiles(client, remotePath, s, true); err == nil {
				progress.SetTotals(s, estimate.Files, estimate.Bytes)
			} else {
				s.Log().WithField("error", err).Debug("failed to pre-scan SFTP files for progress")
			}
			progress.SetStatus(s, "transferring")
		}
	}

	if dstLocation != "" && dstLocation != "/" {
		if err := s.Filesystem().CreateDirectory("", dstLocation); err != nil {
			s.Log().WithField("error", err).Warn("Could not create target directory, it may already exist: " + dstLocation)
		}
	}

	return s.walkAndDownloadSFTP(client, remotePath, dstLocation, false, "", true)
}

func (s *Server) importSelectedSFTP(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, srcLocation, dstLocation string, selectedItems []string, sessionKeys *SessionHostKeyStore) error {
	client, conn, err := s.connectSFTP(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, sessionKeys)
	if err != nil {
		return err
	}
	defer client.Close()
	defer conn.Close()

	srcLocation, dstLocation = normalizePathsForTransfer(srcLocation, dstLocation)
	targetPath := strings.TrimSuffix(dstLocation, "/")
	isRoot := targetPath == "" || targetPath == "/"

	if !isRoot && targetPath != "" {
		if err := s.Filesystem().CreateDirectory("", targetPath); err != nil {
			s.Log().WithField("error", err).Warn("Could not create target directory, it may already exist: " + targetPath)
		}
	}

	if progress := getImportProgress(s.ID()); progress != nil {
		if progress.ShouldScan() {
			estimate := ImportEstimate{}
			for _, item := range selectedItems {
				remotePath, _ := resolveSelectedItemPath(srcLocation, item)
				if remotePath == "" {
					continue
				}
				count, err := countSFTPFiles(client, remotePath, s, false)
				if err != nil {
					return err
				}
				estimate.Files += count.Files
				estimate.Bytes += count.Bytes
			}
			if estimate.Files > 0 {
				progress.SetTotals(s, estimate.Files, estimate.Bytes)
			}
			progress.SetStatus(s, "transferring")
		}
	}

	for _, item := range selectedItems {
		remotePath, relativeItem := resolveSelectedItemPath(srcLocation, item)
		if remotePath == "" {
			continue
		}

		info, err := client.Stat(remotePath)
		if err != nil {
			s.Log().WithField("error", err).Error("Unable to access remote path: " + remotePath)
			continue
		}

		if info.IsDir() {
			if err := s.walkAndDownloadSFTP(client, remotePath, targetPath, isRoot, relativeItem, false); err != nil {
				return err
			}
		} else {
			var targetFilePath string
			if relativeItem == "" {
				relativeItem = filepath.Base(remotePath)
			}
			if isRoot {
				targetFilePath = filepath.Clean(relativeItem)
			} else {
				targetFilePath = filepath.Join(targetPath, filepath.Clean(relativeItem))
			}

			if err := s.downloadFileSFTP(client, remotePath, targetFilePath); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Server) walkAndDownloadSFTP(client *sftp.Client, remotePath, targetPath string, isRoot bool, selectedItem string, skipPerm bool) error {
	return walkDirSFTP(client, remotePath, s, skipPerm, func(path string, info os.FileInfo) error {
		var targetItemPath string

		if selectedItem != "" && strings.Count(selectedItem, "/") > 1 {
			relPath := strings.TrimPrefix(path, remotePath)
			relPath = strings.TrimPrefix(relPath, "/")
			if isRoot {
				targetItemPath = relPath
			} else {
				targetItemPath = filepath.Join(targetPath, relPath)
			}
		} else {
			relPath := strings.TrimPrefix(path, filepath.Dir(remotePath))
			relPath = strings.TrimPrefix(relPath, "/")
			if isRoot {
				targetItemPath = relPath
			} else {
				targetItemPath = filepath.Join(targetPath, relPath)
			}
		}

		if info.IsDir() {
			if targetItemPath != "" {
				return s.Filesystem().CreateDirectory("", targetItemPath)
			}
			return nil
		}

		if err := s.downloadFileSFTP(client, path, targetItemPath); err != nil {
			if skipPerm && isPermissionDenied(err) {
				s.Log().WithField("path", path).Debug("Skipping unreadable file during import")
				return nil
			}
			return err
		}
		return nil
	})
}

func (s *Server) downloadSingleFileSFTP(client *sftp.Client, remotePath, dstLocation string) error {
	fileName := filepath.Base(remotePath)
	targetPath := fileName
	if dstLocation != "" {
		targetPath = filepath.Join(dstLocation, fileName)
	}
	return s.downloadFileSFTP(client, remotePath, filepath.Clean(targetPath))
}

func (s *Server) downloadFileSFTP(client *sftp.Client, remotePath, localPath string) error {
	remotePath = strings.TrimSuffix(remotePath, "/")
	localPath = strings.TrimSuffix(localPath, "/")

	if !strings.HasPrefix(remotePath, "/") {
		remotePath = "/" + remotePath
	}

	srcFile, err := client.OpenFile(remotePath, os.O_RDONLY)
	if err != nil {
		s.Log().WithField("error", err).Error("Unable to open remote file: " + remotePath)
		return err
	}
	defer srcFile.Close()

	srcFileInfo, err := srcFile.Stat()
	if err != nil {
		s.Log().WithField("error", err).Error("Unable to get source file info")
		return err
	}

	if lastSlash := strings.LastIndex(localPath, "/"); lastSlash != -1 {
		parentDir := filepath.Clean(localPath[:lastSlash])
		if err := s.Filesystem().CreateDirectory("", parentDir); err != nil {
			s.Log().WithField("error", err).Error("Unable to create parent directory: " + parentDir)
			return err
		}
	}

	reader := newImportIdleReader(srcFile, importTransferIdleTimeout, srcFile.Close)
	if err := s.Filesystem().Write(filepath.Clean(localPath), reader, srcFileInfo.Size(), 0644); err != nil {
		s.Log().WithField("error", err).Error("Unable to write to local file")
		return err
	}

	if progress := getImportProgress(s.ID()); progress != nil {
		progress.IncrementProcessed(s, remotePath, srcFileInfo.Size())
	}

	return nil
}

func (s *Server) importFullSCP(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, srcLocation, dstLocation string, sessionKeys *SessionHostKeyStore) error {
	conn, err := s.connectSSH(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, sessionKeys)
	if err != nil {
		return err
	}
	defer conn.Close()

	srcLocation, dstLocation = normalizePaths(srcLocation, dstLocation)
	targetPath := strings.TrimSuffix(dstLocation, "/")
	if targetPath != "" && targetPath != "/" {
		if err := s.Filesystem().CreateDirectory("", targetPath); err != nil {
			s.Log().WithField("error", err).Warn("Could not create target directory, it may already exist: " + targetPath)
		}
	} else if targetPath == "/" {
		targetPath = ""
	}

	if progress := getImportProgress(s.ID()); progress != nil {
		if progress.ShouldScan() {
			if estimate, err := s.countSCPFiles(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, srcLocation, true, sessionKeys); err == nil {
				progress.SetTotals(s, estimate.Files, estimate.Bytes)
			} else {
				s.Log().WithField("error", err).Debug("failed to pre-scan SCP files for progress")
			}
			progress.SetStatus(s, "transferring")
		}
	}

	return s.scpDownloadRecursive(conn, srcLocation, targetPath)
}

func (s *Server) importSelectedSCP(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, srcLocation, dstLocation string, selectedItems []string, sessionKeys *SessionHostKeyStore) error {
	conn, err := s.connectSSH(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, sessionKeys)
	if err != nil {
		return err
	}
	defer conn.Close()

	srcLocation, dstLocation = normalizePaths(srcLocation, dstLocation)
	targetPath := strings.TrimSuffix(dstLocation, "/")
	if targetPath != "" && targetPath != "/" {
		if err := s.Filesystem().CreateDirectory("", targetPath); err != nil {
			s.Log().WithField("error", err).Warn("Could not create target directory, it may already exist: " + targetPath)
		}
	} else if targetPath == "/" {
		targetPath = ""
	}

	if progress := getImportProgress(s.ID()); progress != nil {
		if progress.ShouldScan() {
			if estimate, err := s.countSelectedSCPFiles(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, srcLocation, selectedItems, sessionKeys); err == nil && estimate.Files > 0 {
				progress.SetTotals(s, estimate.Files, estimate.Bytes)
			} else if err != nil {
				s.Log().WithField("error", err).Debug("failed to pre-scan SCP files for progress")
			}
			progress.SetStatus(s, "transferring")
		}
	}

	for _, item := range selectedItems {
		remotePath, relativeItem := resolveSelectedItemPath(srcLocation, item)
		if remotePath == "" {
			continue
		}
		localBase := targetPath
		if relativeItem != "" && strings.Contains(relativeItem, "/") {
			localBase = filepath.Join(targetPath, filepath.Dir(relativeItem))
		}
		if localBase != "" {
			if err := s.Filesystem().CreateDirectory("", localBase); err != nil {
				s.Log().WithField("error", err).Warn("Could not create target directory, it may already exist: " + localBase)
			}
		}
		if err := s.scpDownloadRecursive(conn, remotePath, localBase); err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) importFullFTP(user, password, host string, port int, srcLocation, dstLocation string) error {
	client, err := s.connectFTP(user, password, host, port)
	if err != nil {
		return err
	}
	defer client.Close()

	srcLocation, dstLocation = normalizePathsForTransfer(srcLocation, dstLocation)
	targetPath := strings.TrimSuffix(dstLocation, "/")
	isRoot := targetPath == "" || targetPath == "/"

	if !isRoot && targetPath != "" {
		if err := s.Filesystem().CreateDirectory("", targetPath); err != nil {
			s.Log().WithField("error", err).Warn("Could not create target directory, it may already exist: " + targetPath)
		}
	}

	if progress := getImportProgress(s.ID()); progress != nil {
		if progress.ShouldScan() {
			if estimate, err := countFTPFiles(client, srcLocation, s, true); err == nil {
				progress.SetTotals(s, estimate.Files, estimate.Bytes)
			} else {
				s.Log().WithField("error", err).Debug("failed to pre-scan FTP files for progress")
			}
			progress.SetStatus(s, "transferring")
		}
	}

	files, err := client.ReadDir(srcLocation)
	if err != nil {
		s.Log().WithField("error", err).Error("Unable to list remote FTP directory: " + srcLocation)
		return err
	}

	for _, f := range files {
		name := f.Name()

		if f.IsDir() {
			var targetDirPath string
			if isRoot {
				targetDirPath = name
			} else {
				targetDirPath = filepath.Join(targetPath, name)
			}

			if err := s.Filesystem().CreateDirectory("", targetDirPath); err != nil {
				s.Log().WithField("error", err).Error("Unable to create directory: " + targetDirPath)
				return err
			}

			if err := s.walkAndDownloadFTP(client, filepath.Join(srcLocation, name), targetDirPath, true); err != nil {
				return err
			}
		} else {
			var targetFilePath string
			if isRoot {
				targetFilePath = name
			} else {
				targetFilePath = filepath.Join(targetPath, name)
			}

			if err := s.downloadFileFTP(client, filepath.Join(srcLocation, name), targetFilePath, f.Size()); err != nil {
				if isPermissionDenied(err) {
					s.Log().WithField("path", filepath.Join(srcLocation, name)).Debug("Skipping unreadable file during FTP import")
					continue
				}
				return err
			}
		}
	}

	return nil
}

func (s *Server) importSelectedFTP(user, password, host string, port int, srcLocation, dstLocation string, selectedItems []string) error {
	client, err := s.connectFTP(user, password, host, port)
	if err != nil {
		return err
	}
	defer client.Close()

	srcLocation, dstLocation = normalizePathsForTransfer(srcLocation, dstLocation)
	targetPath := strings.TrimSuffix(dstLocation, "/")
	isRoot := targetPath == "" || targetPath == "/"

	if !isRoot && targetPath != "" {
		if err := s.Filesystem().CreateDirectory("", targetPath); err != nil {
			s.Log().WithField("error", err).Warn("Could not create target directory, it may already exist: " + targetPath)
		}
	}

	if progress := getImportProgress(s.ID()); progress != nil {
		if progress.ShouldScan() {
			estimate := ImportEstimate{}
			for _, item := range selectedItems {
				remotePath, _ := resolveSelectedItemPath(srcLocation, item)
				if remotePath == "" {
					continue
				}
				count, err := countFTPFiles(client, remotePath, s, false)
				if err != nil {
					return err
				}
				estimate.Files += count.Files
				estimate.Bytes += count.Bytes
			}
			if estimate.Files > 0 {
				progress.SetTotals(s, estimate.Files, estimate.Bytes)
			}
			progress.SetStatus(s, "transferring")
		}
	}

	for _, item := range selectedItems {
		remotePath, relativeItem := resolveSelectedItemPath(srcLocation, item)
		if remotePath == "" {
			continue
		}

		fileInfo, err := client.Stat(remotePath)
		if err != nil {
			s.Log().WithField("error", err).Error("Unable to access remote path: " + remotePath)
			continue
		}

		if fileInfo.IsDir() {
			err = walkDirFTP(client, remotePath, s, func(path string, info os.FileInfo) error {
				var targetItemPath string

				if strings.Count(relativeItem, "/") > 0 {
					relPath := strings.TrimPrefix(path, remotePath)
					relPath = strings.TrimPrefix(relPath, "/")
					if isRoot {
						targetItemPath = relPath
					} else {
						targetItemPath = filepath.Join(targetPath, relPath)
					}
				} else {
					relPath := strings.TrimPrefix(path, filepath.Dir(remotePath))
					relPath = strings.TrimPrefix(relPath, "/")
					if isRoot {
						targetItemPath = relPath
					} else {
						targetItemPath = filepath.Join(targetPath, relPath)
					}
				}

				if info.IsDir() {
					if targetItemPath != "" {
						return s.Filesystem().CreateDirectory("", targetItemPath)
					}
					return nil
				}
				return s.downloadFileFTP(client, path, targetItemPath, info.Size())
			})
			if err != nil {
				return err
			}
		} else {
			var targetFilePath string
			if relativeItem == "" {
				relativeItem = filepath.Base(remotePath)
			}
			if isRoot {
				targetFilePath = filepath.Clean(relativeItem)
			} else {
				targetFilePath = filepath.Join(targetPath, filepath.Clean(relativeItem))
			}

			if err := s.downloadFileFTP(client, remotePath, targetFilePath, fileInfo.Size()); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Server) walkAndDownloadFTP(client *goftp.Client, remotePath, targetPath string, skipPerm bool) error {
	files, err := client.ReadDir(remotePath)
	if err != nil {
		if skipPerm && isPermissionDenied(err) {
			s.Log().WithField("dir", remotePath).Debug("Skipping unreadable FTP directory during import")
			return nil
		}
		s.Log().WithField("error", err).Error("Unable to list FTP directory: " + remotePath)
		return err
	}

	for _, f := range files {
		name := f.Name()

		if f.IsDir() {
			subDirPath := filepath.Join(targetPath, name)
			if err := s.Filesystem().CreateDirectory("", subDirPath); err != nil {
				s.Log().WithField("error", err).Error("Unable to create directory: " + subDirPath)
				return err
			}
			if err := s.walkAndDownloadFTP(client, filepath.Join(remotePath, name), subDirPath, skipPerm); err != nil {
				return err
			}
		} else {
			targetFilePath := filepath.Join(targetPath, name)
			if err := s.downloadFileFTP(client, filepath.Join(remotePath, name), targetFilePath, f.Size()); err != nil {
				if skipPerm && isPermissionDenied(err) {
					s.Log().WithField("path", filepath.Join(remotePath, name)).Debug("Skipping unreadable file during FTP import")
					continue
				}
				return err
			}
		}
	}

	return nil
}

func (s *Server) downloadFileFTP(client *goftp.Client, remotePath, localPath string, fileSize int64) error {
	if lastSlash := strings.LastIndex(localPath, "/"); lastSlash != -1 {
		parentDir := filepath.Clean(localPath[:lastSlash])
		if err := s.Filesystem().CreateDirectory("", parentDir); err != nil {
			s.Log().WithField("error", err).Error("Unable to create parent directory: " + parentDir)
			return err
		}
	}

	if fileSize <= 0 {

		tempFile, err := os.CreateTemp("", "pterodactyl-ftp-*")
		if err != nil {
			return err
		}
		defer os.Remove(tempFile.Name())
		defer tempFile.Close()

		ftpPath := strings.TrimPrefix(remotePath, "/")
		activity := make(chan struct{}, 1)
		retrieveErr := make(chan error, 1)
		go func() {
			retrieveErr <- client.Retrieve(ftpPath, &importIdleWriter{writer: tempFile, activity: activity})
		}()

		timer := time.NewTimer(importTransferIdleTimeout)
		defer timer.Stop()
		for {
			select {
			case err := <-retrieveErr:
				if err != nil {
					s.Log().WithField("error", err).Error("Failed to retrieve file: " + remotePath)
					return err
				}
				goto ftpRetrieveComplete
			case <-activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(importTransferIdleTimeout)
			case <-timer.C:
				_ = client.Close()
				return fmt.Errorf("import stalled: no data received for %s", importTransferIdleTimeout)
			}
		}

	ftpRetrieveComplete:
		if _, err = tempFile.Seek(0, 0); err != nil {
			return err
		}

		fileInfo, err := tempFile.Stat()
		if err != nil {
			return err
		}

		if err := s.Filesystem().Write(localPath, tempFile, fileInfo.Size(), 0644); err != nil {
			s.Log().WithField("error", err).Error("Failed to write file: " + localPath)
			return err
		}
		if progress := getImportProgress(s.ID()); progress != nil {
			progress.IncrementProcessed(s, remotePath, fileInfo.Size())
		}
		return nil
	}

	reader, writer := io.Pipe()
	retrieveErr := make(chan error, 1)

	go func() {
		ftpPath := strings.TrimPrefix(remotePath, "/")
		err := client.Retrieve(ftpPath, writer)
		_ = writer.CloseWithError(err)
		retrieveErr <- err
	}()

	idleReader := newImportIdleReader(reader, importTransferIdleTimeout, func() error {
		_ = reader.CloseWithError(fmt.Errorf("import stalled: no data received for %s", importTransferIdleTimeout))
		return client.Close()
	})
	writeErr := s.Filesystem().Write(localPath, idleReader, fileSize, 0644)
	if writeErr != nil {
		_ = reader.CloseWithError(writeErr)
	}

	if err := <-retrieveErr; err != nil {
		s.Log().WithField("error", err).Error("Failed to retrieve file: " + remotePath)
		if writeErr == nil {
			return err
		}
	}

	if writeErr != nil {
		s.Log().WithField("error", writeErr).Error("Failed to write file: " + localPath)
		return writeErr
	}

	if progress := getImportProgress(s.ID()); progress != nil {
		progress.IncrementProcessed(s, remotePath, fileSize)
	}

	return nil
}

func (s *Server) connectSFTP(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, sessionKeys *SessionHostKeyStore) (*sftp.Client, *ssh.Client, error) {
	conn, err := s.connectSSH(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, sessionKeys)
	if err != nil {
		return nil, nil, err
	}

	client, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		s.Log().WithField("error", err).Error("Unable to start SFTP subsystem")
		return nil, nil, err
	}

	return client, conn, nil
}

func (s *Server) connectSSH(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, sessionKeys *SessionHostKeyStore) (*ssh.Client, error) {
	authMethods := []ssh.AuthMethod{}
	if strings.TrimSpace(sshKey) != "" {
		var signer ssh.Signer
		var err error
		if sshKeyPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(sshKey), []byte(sshKeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(sshKey))
		}
		if err != nil {
			s.Log().WithField("error", err).Error("Failed to parse SSH private key")
			return nil, err
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}
	if password != "" {
		authMethods = append(authMethods, ssh.Password(password))
	}
	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no SSH authentication method provided")
	}

	resolvedIP, err := validateRemoteHost(host)
	if err != nil {
		return nil, err
	}

	hostKeyCallback, err := resolveHostKeyCallback(hostKeyFingerprint, sessionKeys, host, port)
	if err != nil {
		return nil, err
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         importConnectionTimeout,
	}

	addr := net.JoinHostPort(resolvedIP, strconv.Itoa(port))
	conn, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		s.Log().WithField("error", err).Error("Failed to connect to [" + addr + "]")
		return nil, err
	}

	return conn, nil
}

func (s *Server) connectFTP(user, password, host string, port int) (*goftp.Client, error) {
	resolvedIP, err := validateRemoteHost(host)
	if err != nil {
		return nil, err
	}

	config := goftp.Config{
		User:               user,
		Password:           password,
		ConnectionsPerHost: 3,
		Timeout:            importConnectionTimeout,
	}

	addr := net.JoinHostPort(resolvedIP, strconv.Itoa(port))
	client, err := goftp.DialConfig(config, addr)
	if err != nil {
		s.Log().WithField("error", err).Error("Failed to connect to [" + addr + "]")
		return nil, err
	}

	return client, nil
}

func (s *Server) ensureServerStopped() error {
	if s.Environment.State() != environment.ProcessOfflineState {
		s.Log().Debug("waiting for server instance to enter a stopped state")
		s.Environment.SetState(environment.ProcessOfflineState)
		if err := s.Environment.WaitForStop(s.Context(), time.Second*10, true); err != nil {
			return err
		}
	}
	return nil
}

func normalizePaths(src, dst string) (string, string) {
	dst = strings.TrimSuffix(dst, "/")
	src = strings.TrimSuffix(src, "/")
	if !strings.HasPrefix(src, "/") {
		src = "/" + src
	}
	return src, dst
}

func normalizePathsForTransfer(src, dst string) (string, string) {
	src = strings.TrimSuffix(strings.TrimPrefix(src, "/"), "/")
	dst = strings.TrimSuffix(strings.TrimPrefix(dst, "/"), "/")
	return src, dst
}

func resolveSelectedItemPath(srcLocation, item string) (string, string) {
	src := strings.TrimSpace(srcLocation)
	if src == "" {
		src = "/"
	}
	if !strings.HasPrefix(src, "/") {
		src = "/" + src
	}
	src = strings.TrimSuffix(src, "/")
	if src == "" {
		src = "/"
	}

	selection := strings.TrimSpace(item)
	if selection == "" {
		return "", ""
	}
	if !strings.HasPrefix(selection, "/") {
		selection = "/" + selection
	}
	selection = path.Clean(selection)

	if src != "/" && !strings.HasPrefix(selection, src) {
		selectionNoSlash := strings.TrimPrefix(selection, "/")
		srcNoSlash := strings.TrimPrefix(src, "/")
		if strings.HasPrefix(selectionNoSlash, srcNoSlash) {
			selection = "/" + selectionNoSlash
		} else if strings.TrimPrefix(item, "/") != "" {
			selection = path.Clean(path.Join(src, strings.TrimPrefix(item, "/")))
		}
	}

	var relative string
	if src == "/" {
		relative = strings.TrimPrefix(selection, "/")
	} else if strings.HasPrefix(selection, src) {
		relative = strings.TrimPrefix(strings.TrimPrefix(selection, src), "/")
	} else {
		relative = strings.TrimPrefix(selection, "/")
	}

	return selection, relative
}

var blockedCIDRs = mustParseCIDRs([]string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"255.255.255.255/32",
	"::/128",
	"::1/128",
	"2001:db8::/32",
	"fc00::/7",
	"fe80::/10",
	"fec0::/10",
	"ff00::/8",
})

func mustParseCIDRs(cidrs []string) []*net.IPNet {
	results := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("invalid CIDR %s: %v", cidr, err))
		}
		results = append(results, network)
	}
	return results
}

func validateRemoteHost(host string) (string, error) {
	normalized := strings.TrimSpace(host)
	if normalized == "" {
		return "", fmt.Errorf("host is required")
	}

	if strings.HasPrefix(normalized, "[") && strings.HasSuffix(normalized, "]") {
		normalized = strings.TrimSuffix(strings.TrimPrefix(normalized, "["), "]")
	}

	if ip := net.ParseIP(normalized); ip != nil {
		if !isPublicIP(ip) {
			return "", fmt.Errorf("host resolves to a non-public IP address")
		}
		return ip.String(), nil
	}

	ips, err := net.LookupIP(normalized)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("host does not resolve to any IPs")
	}

	var resolved string
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return "", fmt.Errorf("host resolves to a non-public IP address")
		}
		if resolved == "" {
			resolved = ip.String()
		}
	}

	if resolved == "" {
		return "", fmt.Errorf("host does not resolve to any public IPs")
	}

	return resolved, nil
}

func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, network := range blockedCIDRs {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, os.ErrPermission) {
		return true
	}

	var statusErr *sftp.StatusError
	if errors.As(err, &statusErr) {
		if statusErr.FxCode() == sftp.ErrSSHFxPermissionDenied {
			return true
		}

		if statusErr.FxCode() == sftp.ErrSSHFxFailure {
			return true
		}
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied")
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
		return true
	}

	var statusErr *sftp.StatusError
	if errors.As(err, &statusErr) {
		if statusErr.FxCode() == sftp.ErrSSHFxNoSuchFile {
			return true
		}
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such file") || strings.Contains(msg, "file does not exist")
}

func resolveHostKeyCallback(expectedFingerprint string, sessionKeys *SessionHostKeyStore, host string, port int) (ssh.HostKeyCallback, error) {
	expected := strings.TrimSpace(expectedFingerprint)
	hostKey := net.JoinHostPort(host, strconv.Itoa(port))

	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if sessionKeys == nil {
			return fmt.Errorf("session host key store is unavailable")
		}

		if expected != "" {
			if !hostKeyMatches(expected, key) {
				actual := hostKeyFingerprint(expected, key)
				return fmt.Errorf("ssh host key mismatch (expected %s, got %s)", expected, actual)
			}
		}

		fingerprint := ssh.FingerprintSHA256(key)
		return sessionKeys.VerifyOrStore(hostKey, fingerprint)
	}, nil
}

func hostKeyMatches(expected string, key ssh.PublicKey) bool {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return true
	}

	upper := strings.ToUpper(expected)
	if strings.HasPrefix(upper, "SHA256:") {
		return strings.EqualFold(expected, ssh.FingerprintSHA256(key))
	}
	if strings.HasPrefix(upper, "MD5:") {
		actual := ssh.FingerprintLegacyMD5(key)
		if !strings.HasPrefix(strings.ToUpper(actual), "MD5:") {
			actual = "MD5:" + actual
		}
		return strings.EqualFold(expected, actual)
	}

	if strings.HasPrefix(expected, "ssh-") {
		parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(expected))
		if err == nil {
			return bytes.Equal(parsed.Marshal(), key.Marshal())
		}
	}

	actual := ssh.FingerprintSHA256(key)
	if strings.EqualFold(expected, actual) {
		return true
	}
	return strings.EqualFold("SHA256:"+expected, actual)
}

func hostKeyFingerprint(expected string, key ssh.PublicKey) string {
	upper := strings.ToUpper(strings.TrimSpace(expected))
	if strings.HasPrefix(upper, "MD5:") {
		actual := ssh.FingerprintLegacyMD5(key)
		if !strings.HasPrefix(strings.ToUpper(actual), "MD5:") {
			return "MD5:" + actual
		}
		return actual
	}
	return ssh.FingerprintSHA256(key)
}

func (s *Server) scpDownloadRecursive(conn *ssh.Client, remotePath, localBase string) error {
	session, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}

	remotePath = strings.TrimSuffix(remotePath, "/")
	if remotePath == "" {
		remotePath = "/"
	}

	cmd := fmt.Sprintf("scp -r -f %s", shellEscape(remotePath))
	if err := session.Start(cmd); err != nil {
		return err
	}

	reader := bufio.NewReader(newImportIdleReader(stdout, importTransferIdleTimeout, session.Close))
	if err := scpSendAck(stdin); err != nil {
		return err
	}

	localBase = strings.TrimSuffix(localBase, "/")
	if localBase == "/" {
		localBase = ""
	}

	localStack := []string{localBase}
	remoteStack := []string{}

	for {
		code, err := scpReadByte(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		switch code {
		case 0:
			continue
		case '\x01', '\x02':
			msg, _ := scpReadLine(reader)
			if msg == "" {
				msg = "unknown SCP error"
			}
			s.Log().WithField("error", msg).Warn("scp transfer reported error; continuing without failing import")
			return nil
		case 'T':
			if _, err := scpReadLine(reader); err != nil {
				return err
			}
			if err := scpSendAck(stdin); err != nil {
				return err
			}
		case 'D':
			line, err := scpReadLine(reader)
			if err != nil {
				return err
			}
			name, _, err := scpParseHeader(line)
			if err != nil {
				return err
			}
			if !scpValidName(name) {
				return fmt.Errorf("invalid SCP directory name: %s", name)
			}

			parent := localStack[len(localStack)-1]
			targetDir := filepath.Join(parent, name)
			if err := s.Filesystem().CreateDirectory("", targetDir); err != nil {
				s.Log().WithField("error", err).Error("Unable to create directory: " + targetDir)
				return err
			}

			localStack = append(localStack, targetDir)
			remoteStack = append(remoteStack, name)
			if err := scpSendAck(stdin); err != nil {
				return err
			}
		case 'E':
			if _, err := scpReadLine(reader); err != nil {
				return err
			}
			if len(localStack) > 1 {
				localStack = localStack[:len(localStack)-1]
			}
			if len(remoteStack) > 0 {
				remoteStack = remoteStack[:len(remoteStack)-1]
			}
			if err := scpSendAck(stdin); err != nil {
				return err
			}
		case 'C':
			line, err := scpReadLine(reader)
			if err != nil {
				return err
			}

			name, size, err := scpParseHeader(line)
			if err != nil {
				return err
			}
			if !scpValidName(name) {
				return fmt.Errorf("invalid SCP filename: %s", name)
			}

			parent := localStack[len(localStack)-1]
			targetFile := filepath.Join(parent, name)
			if dir := filepath.Dir(targetFile); dir != "." && dir != "" {
				if err := s.Filesystem().CreateDirectory("", dir); err != nil {
					s.Log().WithField("error", err).Error("Unable to create parent directory: " + dir)
					return err
				}
			}

			if err := scpSendAck(stdin); err != nil {
				return err
			}

			limited := &io.LimitedReader{R: reader, N: size}
			if err := s.Filesystem().Write(filepath.Clean(targetFile), limited, size, 0644); err != nil {
				if limited.N > 0 {
					_, _ = io.CopyN(io.Discard, limited, limited.N)
				}
				return err
			}
			if limited.N > 0 {
				_, _ = io.CopyN(io.Discard, limited, limited.N)
			}

			if err := scpReadAck(reader); err != nil {
				s.Log().WithField("error", err).Warn("scp transfer reported error; continuing without failing import")
				return nil
			}
			if err := scpSendAck(stdin); err != nil {
				return err
			}

			if progress := getImportProgress(s.ID()); progress != nil {
				remoteName := name
				if len(remoteStack) > 0 {
					remoteName = filepath.Join(append(remoteStack, name)...)
				}
				progress.IncrementProcessed(s, remoteName, size)
			}
		default:
			errBuf, _ := io.ReadAll(stderr)
			s.Log().WithFields(log.Fields{
				"code":   code,
				"stderr": strings.TrimSpace(string(errBuf)),
			}).Warn("unexpected SCP response; continuing without failing import")
			return nil
		}
	}

	if err := session.Wait(); err != nil {
		errBuf, _ := io.ReadAll(stderr)
		s.Log().WithFields(log.Fields{
			"error":  err,
			"stderr": strings.TrimSpace(string(errBuf)),
		}).Warn("scp transfer reported errors; continuing without failing import")
		return nil
	}

	return nil
}

func scpReadByte(r *bufio.Reader) (byte, error) {
	return r.ReadByte()
}

func scpReadLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

func scpReadAck(r *bufio.Reader) error {
	b, err := r.ReadByte()
	if err != nil {
		return err
	}
	if b == 0 {
		return nil
	}
	if b == '\x01' || b == '\x02' {
		msg, _ := scpReadLine(r)
		if msg == "" {
			msg = "unknown SCP error"
		}
		return fmt.Errorf("scp error: %s", msg)
	}
	return fmt.Errorf("unexpected SCP ack: %q", b)
}

func scpSendAck(w io.Writer) error {
	_, err := w.Write([]byte{0})
	return err
}

func scpParseHeader(line string) (string, int64, error) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 3 {
		return "", 0, fmt.Errorf("invalid SCP header: %s", line)
	}
	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, err
	}
	return parts[2], size, nil
}

func scpValidName(name string) bool {
	if name == "" || strings.Contains(name, "\x00") {
		return false
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	return true
}

func shellEscape(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func countSFTPFiles(client *sftp.Client, remotePath string, s *Server, skipPerm bool) (ImportEstimate, error) {
	remotePath = strings.TrimSuffix(remotePath, "/")
	if remotePath == "" {
		remotePath = "/"
	}
	if !strings.HasPrefix(remotePath, "/") {
		remotePath = "/" + remotePath
	}

	info, err := client.Stat(remotePath)
	if err != nil {
		if skipPerm && isPermissionDenied(err) {
			return ImportEstimate{}, nil
		}
		if isNotFoundError(err) {
			return ImportEstimate{}, nil
		}
		return ImportEstimate{}, err
	}

	if !info.IsDir() {
		return ImportEstimate{Files: 1, Bytes: info.Size()}, nil
	}

	estimate := ImportEstimate{}
	err = walkDirSFTP(client, remotePath, s, skipPerm, func(path string, info os.FileInfo) error {
		if info.IsDir() {
			return nil
		}
		estimate.Files++
		estimate.Bytes += info.Size()
		if progress := getImportProgress(s.ID()); progress != nil && progress.ShouldScan() {
			progress.SetScanEstimate(s, estimate.Files, estimate.Bytes, path)
		}
		return nil
	})
	if err != nil {
		return ImportEstimate{}, err
	}

	return estimate, nil
}

func countFTPFiles(client *goftp.Client, remotePath string, s *Server, skipPerm bool) (ImportEstimate, error) {
	remotePath = strings.TrimSuffix(remotePath, "/")
	if remotePath == "" {
		remotePath = "/"
	}

	info, err := client.Stat(remotePath)
	if err != nil {
		if skipPerm && isPermissionDenied(err) {
			return ImportEstimate{}, nil
		}
		return ImportEstimate{}, err
	}

	if !info.IsDir() {
		return ImportEstimate{Files: 1, Bytes: info.Size()}, nil
	}

	estimate := ImportEstimate{}
	var walk func(dir string) error
	walk = func(dir string) error {
		files, err := client.ReadDir(dir)
		if err != nil {
			if skipPerm && isPermissionDenied(err) {
				s.Log().WithField("dir", dir).Debug("Skipping unreadable FTP directory during import")
				return nil
			}
			return err
		}

		for _, f := range files {
			if f.IsDir() {
				if err := walk(filepath.Join(dir, f.Name())); err != nil {
					return err
				}
			} else {
				estimate.Files++
				estimate.Bytes += f.Size()
				if progress := getImportProgress(s.ID()); progress != nil && progress.ShouldScan() {
					progress.SetScanEstimate(s, estimate.Files, estimate.Bytes, filepath.Join(dir, f.Name()))
				}
			}
		}
		return nil
	}

	if err := walk(remotePath); err != nil {
		return ImportEstimate{}, err
	}

	return estimate, nil
}

func (s *Server) countSCPFiles(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, srcLocation string, skipPerm bool, sessionKeys *SessionHostKeyStore) (ImportEstimate, error) {
	client, conn, err := s.connectSFTP(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, sessionKeys)
	if err != nil {
		return ImportEstimate{}, err
	}
	defer client.Close()
	defer conn.Close()

	srcLocation = strings.TrimSuffix(srcLocation, "/")
	if srcLocation == "" {
		srcLocation = "/"
	}

	return countSFTPFiles(client, srcLocation, s, skipPerm)
}

func (s *Server) countSelectedSCPFiles(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host string, port int, srcLocation string, selectedItems []string, sessionKeys *SessionHostKeyStore) (ImportEstimate, error) {
	client, conn, err := s.connectSFTP(user, password, sshKey, sshKeyPassphrase, hostKeyFingerprint, host, port, sessionKeys)
	if err != nil {
		return ImportEstimate{}, err
	}
	defer client.Close()
	defer conn.Close()

	estimate := ImportEstimate{}
	for _, item := range selectedItems {
		remotePath, _ := resolveSelectedItemPath(srcLocation, item)
		if remotePath == "" {
			continue
		}
		count, err := countSFTPFiles(client, remotePath, s, false)
		if err != nil {
			s.Log().WithField("error", err).Debug("failed to pre-scan SCP selection for progress")
			continue
		}
		estimate.Files += count.Files
		estimate.Bytes += count.Bytes
	}

	return estimate, nil
}

func walkDirSFTP(client *sftp.Client, dir string, s *Server, skipPerm bool, callback func(path string, info os.FileInfo) error) error {
	s.Log().WithField("dir", dir).Debug("Walking SFTP directory")

	files, err := client.ReadDir(dir)
	if err != nil {
		if isNotFoundError(err) {
			s.Log().WithField("dir", dir).Debug("Skipping missing directory during import")
			return nil
		}
		if skipPerm && isPermissionDenied(err) {
			s.Log().WithField("dir", dir).Debug("Skipping unreadable directory during import")
			return nil
		}
		s.Log().WithField("error", err).Error("Unable to list directory: " + dir)
		return err
	}

	for _, f := range files {
		path := filepath.Join(dir, f.Name())
		path = strings.TrimPrefix(path, "/")

		if err := callback(path, f); err != nil {
			return err
		}

		if f.IsDir() {
			if err := walkDirSFTP(client, path, s, skipPerm, callback); err != nil {
				return err
			}
		}
	}

	return nil
}

func walkDirFTP(client *goftp.Client, dir string, s *Server, callback func(path string, info os.FileInfo) error) error {
	s.Log().WithField("dir", dir).Debug("Walking FTP directory")

	files, err := client.ReadDir(dir)
	if err != nil {
		s.Log().WithField("error", err).Error("Unable to list directory: " + dir)
		return err
	}

	for _, f := range files {
		path := filepath.Join(dir, f.Name())

		if err := callback(path, f); err != nil {
			return err
		}

		if f.IsDir() {
			if err := walkDirFTP(client, path, s, callback); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Server) notifyImportComplete(importErr error) error {

	cfg := config.Get()
	url := fmt.Sprintf("%s/api/remote/servers/%s/importer/complete",
		strings.TrimSuffix(cfg.PanelLocation, "/"),
		s.ID())

	payload := map[string]interface{}{
		"successful": importErr == nil,
	}
	if importErr != nil {
		payload["error"] = importErr.Error()
	} else {
		payload["message"] = "Import completed successfully."
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s.%s", cfg.AuthenticationTokenId, cfg.AuthenticationToken))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(responseBody) > 0 {
			return fmt.Errorf("panel returned status %d when clearing import status: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
		}

		return fmt.Errorf("panel returned status %d when clearing import status", resp.StatusCode)
	}

	return nil
}
