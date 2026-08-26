package router

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/models"
	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/router/middleware"
	"github.com/pterodactyl/wings/router/tokens"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/server/filesystem"
)

const (
	maxConcurrentResumableUploads          = 64
	maxConcurrentResumableUploadsPerServer = 16
	maxConcurrentResumableUploadsPerToken  = 4
	maxResumableUploadLocks                = maxConcurrentResumableUploads + 64
	maxResumableUploadSessions             = 4096
	maxResumableUploadSessionsPerServer    = 256
	resumableUploadIdleTimeout             = 30 * time.Second
)

var errResumableUploadIdleTimeout = errors.New("resumable upload body timed out")

type resumableUploadIdleTimeoutContextKey struct{}

type resumableUploadLockRegistry struct {
	mu     sync.Mutex
	active map[string]struct{}
}

type resumableUploadAdmissionRegistry struct {
	mu             sync.Mutex
	globalLimit    int
	perServerLimit int
	perTokenLimit  int
	global         int
	byServer       map[string]int
	byToken        map[string]int
}

type resumableUploadSession struct {
	serverID string
	target   string
	expires  time.Time
	consumed bool
}

type resumableUploadSessionRegistry struct {
	mu             sync.Mutex
	globalLimit    int
	perServerLimit int
	nextExpiry     time.Time
	sessions       map[string]resumableUploadSession
	byServer       map[string]int
}

var uploadLocks = resumableUploadLockRegistry{active: make(map[string]struct{})}

var uploadAdmissions = newResumableUploadAdmissionRegistry(
	maxConcurrentResumableUploads,
	maxConcurrentResumableUploadsPerServer,
	maxConcurrentResumableUploadsPerToken,
)

var uploadSessions = newResumableUploadSessionRegistry(
	maxResumableUploadSessions,
	maxResumableUploadSessionsPerServer,
)

var uploadBufferPool = sync.Pool{New: func() interface{} {
	buffer := make([]byte, 64*1024)
	return &buffer
}}

var saveResumableUploadActivity = func(s *server.Server, user, ip, filename, directory string) {
	s.SaveActivity(s.NewRequestActivity(user, ip), server.ActivityFileUploaded, models.ActivityMeta{
		"file":      filename,
		"directory": directory,
	})
}

func (r *resumableUploadLockRegistry) acquire(key string) (func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.active[key]; ok || len(r.active) >= maxResumableUploadLocks {
		return nil, false
	}
	r.active[key] = struct{}{}

	return func() {
		r.mu.Lock()
		delete(r.active, key)
		r.mu.Unlock()
	}, true
}

func newResumableUploadAdmissionRegistry(global, perServer, perToken int) *resumableUploadAdmissionRegistry {
	return &resumableUploadAdmissionRegistry{
		globalLimit:    global,
		perServerLimit: perServer,
		perTokenLimit:  perToken,
		byServer:       make(map[string]int),
		byToken:        make(map[string]int),
	}
}

func (r *resumableUploadAdmissionRegistry) acquire(serverID, tokenID string) (func(), bool) {
	tokenKey := serverID + "\x00" + tokenID
	r.mu.Lock()
	if r.global >= r.globalLimit || r.byServer[serverID] >= r.perServerLimit || r.byToken[tokenKey] >= r.perTokenLimit {
		r.mu.Unlock()
		return nil, false
	}
	r.global++
	r.byServer[serverID]++
	r.byToken[tokenKey]++
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.global--
			r.byServer[serverID]--
			if r.byServer[serverID] == 0 {
				delete(r.byServer, serverID)
			}
			r.byToken[tokenKey]--
			if r.byToken[tokenKey] == 0 {
				delete(r.byToken, tokenKey)
			}
			r.mu.Unlock()
		})
	}, true
}

func newResumableUploadSessionRegistry(global, perServer int) *resumableUploadSessionRegistry {
	return &resumableUploadSessionRegistry{
		globalLimit:    global,
		perServerLimit: perServer,
		sessions:       make(map[string]resumableUploadSession),
		byServer:       make(map[string]int),
	}
}

func (r *resumableUploadSessionRegistry) bind(token *tokens.UploadPayload, serverID, target string) bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneExpired(now)

	if session, ok := r.sessions[token.UniqueId]; ok {
		return !session.consumed && session.serverID == serverID && session.target == target
	}
	if len(r.sessions) >= r.globalLimit || r.byServer[serverID] >= r.perServerLimit {
		return false
	}
	if token.ExpirationTime == nil || !now.Before(token.ExpirationTime.Time) || !token.IsUniqueRequest() {
		return false
	}

	expires := token.ExpirationTime.Time
	r.sessions[token.UniqueId] = resumableUploadSession{
		serverID: serverID,
		target:   target,
		expires:  expires,
	}
	r.byServer[serverID]++
	if r.nextExpiry.IsZero() || expires.Before(r.nextExpiry) {
		r.nextExpiry = expires
	}
	return true
}

func (r *resumableUploadSessionRegistry) retire(tokenID, serverID, target string) {
	r.mu.Lock()
	if session, ok := r.sessions[tokenID]; ok && !session.consumed && session.serverID == serverID && session.target == target {
		session.target = ""
		session.consumed = true
		r.sessions[tokenID] = session
	}
	r.mu.Unlock()
}

func (r *resumableUploadSessionRegistry) pruneExpired(now time.Time) {
	if r.nextExpiry.IsZero() || now.Before(r.nextExpiry) {
		return
	}
	r.nextExpiry = time.Time{}
	for id, session := range r.sessions {
		if !now.Before(session.expires) {
			delete(r.sessions, id)
			r.byServer[session.serverID]--
			if r.byServer[session.serverID] == 0 {
				delete(r.byServer, session.serverID)
			}
			continue
		}
		if r.nextExpiry.IsZero() || session.expires.Before(r.nextExpiry) {
			r.nextExpiry = session.expires
		}
	}
}

func headServerUploadFile(c *gin.Context) {
	s, token, ok := resumableUploadServer(c)
	if !ok {
		return
	}
	directory, target, ok := resumableUploadTarget(c, s)
	_ = directory
	if !ok {
		return
	}
	if !uploadSessions.bind(token, s.ID(), target) {
		abortResumableUploadFile(c)
		return
	}

	release, ok := uploadLocks.acquire(s.ID() + "\x00" + target)
	if !ok {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Too many uploads are currently active."})
		return
	}
	defer release()

	offset, err := resumableUploadOffset(s.Filesystem(), target)
	if err != nil {
		abortResumableUploadFile(c)
		return
	}
	c.Header("Upload-Offset", strconv.FormatInt(offset, 10))
	c.Status(http.StatusOK)
}

func patchServerUploadFile(c *gin.Context) {
	s, token, ok := resumableUploadServer(c)
	if !ok {
		return
	}
	directory, target, ok := resumableUploadTarget(c, s)
	if !ok {
		return
	}

	if contentType := c.GetHeader("Content-Type"); contentType != "application/offset+octet-stream" {
		c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": "Content-Type must be application/offset+octet-stream."})
		return
	}
	requestedOffset, ok := parseUploadInteger(c, "Upload-Offset", true)
	if !ok {
		return
	}
	uploadLength, hasUploadLength, ok := parseOptionalUploadInteger(c, "Upload-Length")
	if !ok {
		return
	}
	complete, ok := parseUploadComplete(c)
	if !ok {
		return
	}

	maxSize, ok := resumableUploadLimit(c)
	if !ok {
		return
	}
	if hasUploadLength && uploadLength > maxSize {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "The upload exceeds the configured upload limit."})
		return
	}
	if !uploadSessions.bind(token, s.ID(), target) {
		abortResumableUploadFile(c)
		return
	}

	releaseAdmission, ok := uploadAdmissions.acquire(s.ID(), token.UniqueId)
	if !ok {
		c.Header("Retry-After", "1")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "Too many resumable uploads are currently active."})
		return
	}
	defer releaseAdmission()

	release, ok := uploadLocks.acquire(s.ID() + "\x00" + target)
	if !ok {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "An upload is already active for this file."})
		return
	}
	defer release()

	current, err := resumableUploadOffset(s.Filesystem(), target)
	if err != nil {
		abortResumableUploadFile(c)
		return
	}
	c.Header("Upload-Offset", strconv.FormatInt(current, 10))
	if requestedOffset != current {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "Upload offset does not match the current file size."})
		return
	}
	if current > maxSize || (hasUploadLength && (uploadLength < current || requestedOffset > uploadLength)) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "Upload length is inconsistent with the current file size."})
		return
	}

	allowed := maxSize - current
	if hasUploadLength && uploadLength-current < allowed {
		allowed = uploadLength - current
	}
	if c.Request.ContentLength >= 0 && c.Request.ContentLength > allowed {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "The upload chunk exceeds the remaining upload length."})
		return
	}
	if err := s.Filesystem().HasSpaceFor(maxInt64(c.Request.ContentLength, 0)); err != nil {
		c.AbortWithStatusJSON(http.StatusInsufficientStorage, gin.H{"error": "The server does not have enough disk space for this upload."})
		return
	}
	if directory != "/" {
		if err := s.Filesystem().CreateDirectory(pathBase(directory), pathDirectory(directory)); err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}
	}

	file, err := s.Filesystem().Touch(target, ufs.O_WRONLY|ufs.O_APPEND|ufs.O_NOFOLLOW)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	rollback := func() {
		_ = file.Truncate(current)
	}

	buffer := uploadBufferPool.Get().(*[]byte)
	defer uploadBufferPool.Put(buffer)
	deadlineReader := newResumableUploadDeadlineReader(
		c.Writer,
		c.Request.Body,
		resumableUploadIdleTimeoutForRequest(c.Request),
	)
	reader := io.LimitReader(&contextUploadReader{ctx: c.Request.Context(), reader: deadlineReader}, allowed+1)
	written, copyErr := io.CopyBuffer(writerOnly{Writer: file}, reader, *buffer)
	if deadlineErr := deadlineReader.clear(); copyErr == nil && deadlineErr != nil {
		copyErr = deadlineErr
	}
	if copyErr != nil || written > allowed {
		rollback()
		_ = file.Close()
		if filesystem.IsErrorCode(copyErr, filesystem.ErrCodeDiskSpace) {
			c.AbortWithStatusJSON(http.StatusInsufficientStorage, gin.H{"error": "The server does not have enough disk space for this upload."})
		} else if written > allowed {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "The upload chunk exceeds the remaining upload length."})
		} else if errors.Is(copyErr, errResumableUploadIdleTimeout) {
			c.AbortWithStatusJSON(http.StatusRequestTimeout, gin.H{"error": "The upload body was idle for too long."})
		} else {
			middleware.CaptureAndAbort(c, copyErr)
		}
		return
	}

	newOffset := current + written
	if complete && hasUploadLength && newOffset != uploadLength {
		rollback()
		_ = file.Close()
		c.Header("Upload-Offset", strconv.FormatInt(current, 10))
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "Final upload length does not match Upload-Length."})
		return
	}
	if err := file.Close(); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	if err := s.Filesystem().Chown(target); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	if complete {
		uploadSessions.retire(token.UniqueId, s.ID(), target)
		saveResumableUploadActivity(s, token.UserUuid, c.ClientIP(), c.Query("file"), directory)
	}

	c.Header("Upload-Offset", strconv.FormatInt(newOffset, 10))
	c.Status(http.StatusOK)
}

func resumableUploadServer(c *gin.Context) (*server.Server, *tokens.UploadPayload, bool) {
	token := &tokens.UploadPayload{}
	if err := tokens.ParseToken([]byte(c.Query("token")), token); err != nil {
		abortResumableUploadFile(c)
		return nil, nil, false
	}
	s, ok := middleware.ExtractManager(c).Get(token.ServerUuid)
	if !ok || token.UniqueId == "" || token.Denylisted() || !token.HasScope(tokens.FileUpload) {
		abortResumableUploadFile(c)
		return nil, nil, false
	}
	return s, token, true
}

func resumableUploadTarget(c *gin.Context, s *server.Server) (string, string, bool) {
	directory, target, err := normalizeBetterFilesUploadPath(c.Query("directory"), c.Query("file"))
	if err != nil || ensureBetterFilesAllowed(s.Filesystem(), target) != nil {
		abortResumableUploadFile(c)
		return "", "", false
	}
	return directory, target, true
}

func resumableUploadOffset(fs *filesystem.Filesystem, target string) (int64, error) {
	info, err := fs.UnixFS().Lstat(target)
	if errors.Is(err, ufs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&ufs.ModeSymlink != 0 {
		return 0, errBetterFilesInvalidPath
	}
	return info.Size(), nil
}

func resumableUploadLimit(c *gin.Context) (int64, bool) {
	megabytes := config.Get().Api.UploadLimit
	const maxMegabytes = int64(^uint64(0)>>1) / (1024 * 1024)
	if megabytes <= 0 || megabytes > maxMegabytes {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Upload limit is not configured correctly."})
		return 0, false
	}
	return megabytes * 1024 * 1024, true
}

func parseUploadInteger(c *gin.Context, name string, required bool) (int64, bool) {
	value := c.GetHeader(name)
	if value == "" && required {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": name + " header is required."})
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": name + " must be a non-negative integer."})
		return 0, false
	}
	return parsed, true
}

func parseOptionalUploadInteger(c *gin.Context, name string) (int64, bool, bool) {
	if c.GetHeader(name) == "" {
		return 0, false, true
	}
	value, ok := parseUploadInteger(c, name, false)
	return value, true, ok
}

func parseUploadComplete(c *gin.Context) (bool, bool) {
	switch c.GetHeader("Upload-Complete") {
	case "", "?0":
		return false, true
	case "?1":
		return true, true
	default:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Upload-Complete must be ?0 or ?1."})
		return false, false
	}
}

func abortResumableUploadFile(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "The requested resource was not found on this server."})
}

type contextUploadReader struct {
	ctx    context.Context
	reader io.Reader
}

type resumableUploadDeadlineReader struct {
	reader     io.Reader
	controller *http.ResponseController
	timeout    time.Duration
	supported  bool
}

func newResumableUploadDeadlineReader(writer http.ResponseWriter, reader io.Reader, timeout time.Duration) *resumableUploadDeadlineReader {
	return &resumableUploadDeadlineReader{
		reader:     reader,
		controller: http.NewResponseController(writer),
		timeout:    timeout,
	}
}

func (r *resumableUploadDeadlineReader) Read(buffer []byte) (int, error) {
	if err := r.setDeadline(time.Now().Add(r.timeout)); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(buffer)
	if err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return n, errors.Join(errResumableUploadIdleTimeout, err)
		}
	}
	return n, err
}

func (r *resumableUploadDeadlineReader) setDeadline(deadline time.Time) error {
	err := r.controller.SetReadDeadline(deadline)
	if err != nil {
		return err
	}
	r.supported = true
	return nil
}

func (r *resumableUploadDeadlineReader) clear() error {
	if !r.supported {
		return nil
	}
	return r.controller.SetReadDeadline(time.Time{})
}

func resumableUploadIdleTimeoutForRequest(request *http.Request) time.Duration {
	if timeout, ok := request.Context().Value(resumableUploadIdleTimeoutContextKey{}).(time.Duration); ok && timeout > 0 {
		return timeout
	}
	return resumableUploadIdleTimeout
}

func (r *contextUploadReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
}

type writerOnly struct{ io.Writer }

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func pathBase(value string) string {
	index := strings.LastIndexByte(value, '/')
	return value[index+1:]
}

func pathDirectory(value string) string {
	index := strings.LastIndexByte(value, '/')
	if index <= 0 {
		return "/"
	}
	return value[:index]
}
