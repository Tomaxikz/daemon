package websocket

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/server"
	"github.com/reearth/ygo/crdt"
)

const (
	// NativeFileCollaborationEnabled is consumed by the authenticated system
	// capability endpoint. Keep it coupled to this implementation so the daemon
	// does not advertise a transport that was omitted from the build.
	NativeFileCollaborationEnabled = true

	FileCollabSubscribeEvent   = Event("file collab subscribe")
	FileCollabUnsubscribeEvent = Event("file collab unsubscribe")
	FileCollabUpdateEvent      = Event("file collab update")
	FileCollabAwarenessEvent   = Event("file collab awareness")
	FileCollabSaveEvent        = Event("file collab save")

	fileCollabSyncEvent         = Event("file collab sync")
	fileCollabParticipantsEvent = Event("file collab participants")
	fileCollabSavedEvent        = Event("file collab saved")
	fileCollabErrorEvent        = Event("file collab error")

	fileCollabMaxAwarenessBytes    = 16 * 1024
	fileCollabMaxSessionsPerServer = 32
	fileCollabMaxSessionsPerSocket = 32
	fileCollabSessionGracePeriod   = 30 * time.Second
	fileCollabDefaultFileSizeCap   = 10 * 1024 * 1024
	fileCollabMinimumFileSizeCap   = 1 * 1024 * 1024
	fileCollabMaximumFileSizeCap   = 64 * 1024 * 1024
)

func NativeFileCollaborationFileSizeCap() int {
	return normalizeNativeFileCollaborationFileSizeCap(config.Get().System.FileCollaboration.FileSizeCap)
}

func NativeFileCollaborationConfigured() bool {
	return NativeFileCollaborationEnabled && config.Get().System.FileCollaboration.Enabled
}

func normalizeNativeFileCollaborationFileSizeCap(configured uint64) int {
	if configured == 0 {
		return fileCollabDefaultFileSizeCap
	}
	// Older panel settings commonly express this value as whole MiB. Preserve
	// that intent instead of interpreting values such as 1 as one byte.
	if configured <= 64 {
		configured *= 1024 * 1024
	}
	if configured < fileCollabMinimumFileSizeCap {
		return fileCollabMinimumFileSizeCap
	}
	if configured > fileCollabMaximumFileSizeCap {
		return fileCollabMaximumFileSizeCap
	}
	return int(configured)
}

var (
	errFileCollabInvalidRequest = errors.New("invalid collaborative editing request")
	errFileCollabNotSubscribed  = errors.New("not subscribed to this file")
	errFileCollabTooLarge       = errors.New("file is too large for collaborative editing")
)

type nativeFileCollabParticipant struct {
	User   string  `json:"user"`
	Name   string  `json:"name"`
	Avatar *string `json:"avatar"`
}

type nativeFileCollabMember struct {
	handler *Handler
	user    nativeFileCollabParticipant
	pending []byte
}

type nativeFileCollabSession struct {
	mu sync.Mutex

	serverUUID         string
	path               string
	doc                *crdt.Doc
	text               *crdt.YText
	dirty              bool
	diskHash           [sha256.Size]byte
	appliedUpdateBytes int64
	members            map[string]*nativeFileCollabMember
	teardownGeneration uint64
	saveMu             sync.Mutex
}

type nativeFileCollabSyncMeta struct {
	Dirty bool `json:"dirty"`
}

type nativeFileCollabSaved struct {
	User       string `json:"user"`
	RevisionID *int64 `json:"revision_id"`
}

var nativeFileCollabRegistry = struct {
	sync.Mutex
	sessions map[string]*nativeFileCollabSession
}{sessions: map[string]*nativeFileCollabSession{}}

// IsNativeFileCollaborationEvent reports whether an event belongs to the
// Wings-rs-compatible Yjs collaboration protocol.
func IsNativeFileCollaborationEvent(event Event) bool {
	switch event {
	case FileCollabSubscribeEvent, FileCollabUnsubscribeEvent, FileCollabUpdateEvent, FileCollabAwarenessEvent, FileCollabSaveEvent:
		return true
	default:
		return false
	}
}

// HandleNativeFileCollaboration implements the native Wings-rs collaboration
// wire protocol using Yjs V1 updates. It intentionally remains separate from
// the legacy Better Files OT protocol while clients migrate.
func (h *Handler) HandleNativeFileCollaboration(ctx context.Context, message Message) (bool, error) {
	switch message.Event {
	case FileCollabSubscribeEvent, FileCollabUnsubscribeEvent, FileCollabUpdateEvent, FileCollabAwarenessEvent, FileCollabSaveEvent:
	default:
		return false, nil
	}
	if !NativeFileCollaborationConfigured() {
		return true, h.nativeFileCollabError("", "collaborative editing is disabled")
	}

	h.fileCollabCleanup.Do(func() {
		go func() {
			<-ctx.Done()
			nativeFileCollabDisconnect(h)
		}()
	})

	if !nativeFileCollabValidArgs(message) {
		errorPath := ""
		if len(message.Args) > 0 {
			errorPath = message.Args[0]
			if normalized, ok := betterFilesCollabPath(errorPath); ok {
				errorPath = normalized
			}
		}
		return true, h.nativeFileCollabError(errorPath, errFileCollabInvalidRequest.Error())
	}
	path, ok := betterFilesCollabPath(message.Args[0])
	if !ok {
		return true, h.nativeFileCollabError(message.Args[0], "file not found")
	}

	requiredPermission := betterFilesCollabReadPermission
	if message.Event == FileCollabUpdateEvent || message.Event == FileCollabSaveEvent {
		requiredPermission = betterFilesCollabWritePermission
	}
	jwt := h.GetJwt()
	if jwt == nil || !jwt.HasPermission(requiredPermission) {
		return true, h.nativeFileCollabError(path, "missing permission")
	}

	var err error
	switch message.Event {
	case FileCollabSubscribeEvent:
		err = nativeFileCollabSubscribe(h, path)
	case FileCollabUnsubscribeEvent:
		err = nativeFileCollabUnsubscribe(h, path)
	case FileCollabUpdateEvent:
		err = nativeFileCollabApplyUpdate(h, path, message.Args[1] == "1", message.Args[2])
	case FileCollabAwarenessEvent:
		err = nativeFileCollabRelayAwareness(h, path, message.Args[1])
	case FileCollabSaveEvent:
		err = nativeFileCollabSave(h, path)
	}
	if err != nil {
		return true, h.nativeFileCollabError(path, err.Error())
	}
	return true, nil
}

func nativeFileCollabValidArgs(message Message) bool {
	switch message.Event {
	case FileCollabSubscribeEvent, FileCollabUnsubscribeEvent, FileCollabSaveEvent:
		return len(message.Args) == 1
	case FileCollabUpdateEvent:
		return len(message.Args) == 3 && (message.Args[1] == "0" || message.Args[1] == "1") && message.Args[2] != ""
	case FileCollabAwarenessEvent:
		return len(message.Args) == 2 && message.Args[1] != ""
	default:
		return false
	}
}

func (h *Handler) nativeFileCollabError(path string, message string) error {
	return h.SendJson(Message{Event: fileCollabErrorEvent, Args: []string{path, message}})
}

func nativeFileCollabSubscribe(h *Handler, path string) error {
	key := nativeFileCollabKey(h.server.ID(), path)
	if nativeFileCollabSubscriptionCount(h) >= fileCollabMaxSessionsPerSocket {
		nativeFileCollabRegistry.Lock()
		existing := nativeFileCollabRegistry.sessions[key]
		nativeFileCollabRegistry.Unlock()
		if existing == nil || !nativeFileCollabHasMember(existing, h) {
			return errors.New("too many collaborative sessions open on this connection")
		}
	}

	content, err := nativeFileCollabReadFile(h, path)
	if err != nil {
		return err
	}

	nativeFileCollabRegistry.Lock()
	session := nativeFileCollabRegistry.sessions[key]
	if session == nil {
		count := 0
		for _, existing := range nativeFileCollabRegistry.sessions {
			if existing.serverUUID == h.server.ID() {
				count++
			}
		}
		if count >= fileCollabMaxSessionsPerServer {
			nativeFileCollabRegistry.Unlock()
			return errors.New("too many collaborative sessions open on this server")
		}
		session = nativeFileCollabNewSession(h.server.ID(), path, content)
		nativeFileCollabRegistry.sessions[key] = session
	}
	nativeFileCollabRegistry.Unlock()

	jwt := h.GetJwt()
	name := strings.TrimSpace(jwt.UserName)
	if name == "" {
		name = jwt.UserUUID
	}
	name = betterFilesCollabSafeText(name, 80)
	avatar := nativeFileCollabSafeAvatar(jwt.UserAvatar)

	session.mu.Lock()
	session.teardownGeneration++
	if !session.dirty && session.diskHash != sha256.Sum256([]byte(content)) {
		session.resetDocument(content)
	}
	session.members[h.Uuid().String()] = &nativeFileCollabMember{
		handler: h,
		user: nativeFileCollabParticipant{
			User:   jwt.UserUUID,
			Name:   name,
			Avatar: avatar,
		},
	}
	state := session.doc.EncodeStateAsUpdate()
	dirty := session.dirty
	session.mu.Unlock()

	meta, _ := json.Marshal(nativeFileCollabSyncMeta{Dirty: dirty})
	if err := h.SendJson(Message{Event: fileCollabSyncEvent, Args: []string{
		path,
		base64.StdEncoding.EncodeToString(state),
		string(meta),
	}}); err != nil {
		return err
	}
	nativeFileCollabBroadcastParticipants(session)
	return nil
}

func nativeFileCollabApplyUpdate(h *Handler, path string, finished bool, encoded string) error {
	session, member := nativeFileCollabSubscribedSession(h, path)
	if session == nil || member == nil {
		return errFileCollabNotSubscribed
	}
	chunk, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("invalid update encoding")
	}

	session.mu.Lock()
	member = session.members[h.Uuid().String()]
	if member == nil {
		session.mu.Unlock()
		return errFileCollabNotSubscribed
	}
	if len(member.pending)+len(chunk) > NativeFileCollaborationFileSizeCap() {
		member.pending = nil
		session.mu.Unlock()
		return errors.New("update is too large")
	}
	member.pending = append(member.pending, chunk...)
	if !finished {
		session.mu.Unlock()
		return nil
	}
	update := append([]byte(nil), member.pending...)
	member.pending = nil
	if len(update) == 0 {
		session.mu.Unlock()
		return errFileCollabInvalidRequest
	}
	session.mu.Unlock()
	needsResync, err := session.applyUpdate(update, h.Uuid().String())
	if err != nil {
		return err
	}

	if needsResync {
		nativeFileCollabBroadcast(session, "", Message{Event: fileCollabErrorEvent, Args: []string{path, "resync"}})
		return nil
	}
	nativeFileCollabBroadcast(session, h.Uuid().String(), Message{Event: FileCollabUpdateEvent, Args: []string{
		path,
		base64.StdEncoding.EncodeToString(update),
	}})
	return nil
}

func nativeFileCollabRelayAwareness(h *Handler, path string, payload string) error {
	session, member := nativeFileCollabSubscribedSession(h, path)
	if session == nil || member == nil {
		return errFileCollabNotSubscribed
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(decoded) > fileCollabMaxAwarenessBytes {
		return errors.New("invalid awareness update")
	}
	nativeFileCollabBroadcast(session, h.Uuid().String(), Message{Event: FileCollabAwarenessEvent, Args: []string{path, payload}})
	return nil
}

func nativeFileCollabSave(h *Handler, path string) error {
	session, member := nativeFileCollabSubscribedSession(h, path)
	if session == nil || member == nil {
		return errFileCollabNotSubscribed
	}
	session.saveMu.Lock()
	defer session.saveMu.Unlock()

	session.mu.Lock()
	content := session.text.ToString()
	session.mu.Unlock()

	before, err := nativeFileCollabReadFile(h, path)
	if err != nil {
		return err
	}
	if err := h.server.Filesystem().Write(path, bytes.NewReader([]byte(content)), int64(len(content)), 0o644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	var revisionID *int64
	if server.ShouldRecordFileHistory(path, uint64(len(content))) {
		id, revisionErr := h.server.RecordFileRevision(path, []byte(before), []byte(content), member.user.User)
		if revisionErr != nil {
			h.server.Log().WithError(revisionErr).WithField("path", path).Warn("failed to record collaborative file revision")
		} else if id > 0 {
			revisionID = &id
		}
	}

	session.mu.Lock()
	session.diskHash = sha256.Sum256([]byte(content))
	session.dirty = session.text.ToString() != content
	session.mu.Unlock()

	payload, _ := json.Marshal(nativeFileCollabSaved{User: member.user.User, RevisionID: revisionID})
	nativeFileCollabBroadcast(session, "", Message{Event: fileCollabSavedEvent, Args: []string{path, string(payload)}})
	return nil
}

func nativeFileCollabUnsubscribe(h *Handler, path string) error {
	key := nativeFileCollabKey(h.server.ID(), path)
	nativeFileCollabRegistry.Lock()
	session := nativeFileCollabRegistry.sessions[key]
	nativeFileCollabRegistry.Unlock()
	if session == nil {
		return nil
	}
	nativeFileCollabRemoveMember(session, h.Uuid().String())
	return nil
}

func nativeFileCollabDisconnect(h *Handler) {
	nativeFileCollabRegistry.Lock()
	sessions := make([]*nativeFileCollabSession, 0)
	for _, session := range nativeFileCollabRegistry.sessions {
		if session.serverUUID == h.server.ID() {
			sessions = append(sessions, session)
		}
	}
	nativeFileCollabRegistry.Unlock()
	for _, session := range sessions {
		nativeFileCollabRemoveMember(session, h.Uuid().String())
	}
}

func nativeFileCollabRemoveMember(session *nativeFileCollabSession, handlerID string) {
	session.mu.Lock()
	if session.members[handlerID] == nil {
		session.mu.Unlock()
		return
	}
	delete(session.members, handlerID)
	empty := len(session.members) == 0
	session.teardownGeneration++
	generation := session.teardownGeneration
	session.mu.Unlock()

	if !empty {
		nativeFileCollabBroadcastParticipants(session)
		return
	}
	time.AfterFunc(fileCollabSessionGracePeriod, func() {
		key := nativeFileCollabKey(session.serverUUID, session.path)
		nativeFileCollabRegistry.Lock()
		session.mu.Lock()
		remove := len(session.members) == 0 && session.teardownGeneration == generation && nativeFileCollabRegistry.sessions[key] == session
		if remove {
			delete(nativeFileCollabRegistry.sessions, key)
			session.doc.Destroy()
		}
		session.mu.Unlock()
		nativeFileCollabRegistry.Unlock()
	})
}

func nativeFileCollabSubscribedSession(h *Handler, path string) (*nativeFileCollabSession, *nativeFileCollabMember) {
	key := nativeFileCollabKey(h.server.ID(), path)
	nativeFileCollabRegistry.Lock()
	session := nativeFileCollabRegistry.sessions[key]
	nativeFileCollabRegistry.Unlock()
	if session == nil {
		return nil, nil
	}
	session.mu.Lock()
	member := session.members[h.Uuid().String()]
	session.mu.Unlock()
	return session, member
}

func nativeFileCollabSubscriptionCount(h *Handler) int {
	nativeFileCollabRegistry.Lock()
	sessions := make([]*nativeFileCollabSession, 0, len(nativeFileCollabRegistry.sessions))
	for _, session := range nativeFileCollabRegistry.sessions {
		if session.serverUUID == h.server.ID() {
			sessions = append(sessions, session)
		}
	}
	nativeFileCollabRegistry.Unlock()
	count := 0
	for _, session := range sessions {
		if nativeFileCollabHasMember(session, h) {
			count++
		}
	}
	return count
}

func nativeFileCollabHasMember(session *nativeFileCollabSession, h *Handler) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.members[h.Uuid().String()] != nil
}

func nativeFileCollabBroadcastParticipants(session *nativeFileCollabSession) {
	session.mu.Lock()
	participants := make([]nativeFileCollabParticipant, 0, len(session.members))
	seen := map[string]struct{}{}
	for _, member := range session.members {
		if _, ok := seen[member.user.User]; ok {
			continue
		}
		seen[member.user.User] = struct{}{}
		participants = append(participants, member.user)
	}
	session.mu.Unlock()
	payload, _ := json.Marshal(participants)
	nativeFileCollabBroadcast(session, "", Message{Event: fileCollabParticipantsEvent, Args: []string{session.path, string(payload)}})
}

func nativeFileCollabBroadcast(session *nativeFileCollabSession, exceptHandlerID string, message Message) {
	session.mu.Lock()
	handlers := make([]*Handler, 0, len(session.members))
	for id, member := range session.members {
		if id != exceptHandlerID {
			handlers = append(handlers, member.handler)
		}
	}
	session.mu.Unlock()
	for _, handler := range handlers {
		_ = handler.SendJson(message)
	}
}

func nativeFileCollabReadFile(h *Handler, path string) (string, error) {
	fileSizeCap := NativeFileCollaborationFileSizeCap()
	if err := h.server.Filesystem().IsIgnored(path); err != nil {
		return "", errors.New("file not found")
	}
	file, stat, err := h.server.Filesystem().File(path)
	if err != nil {
		return "", errors.New("file not found")
	}
	defer file.Close()
	if stat.IsDir() {
		return "", errors.New("file is not a file")
	}
	if stat.Size() > int64(fileSizeCap) {
		return "", errFileCollabTooLarge
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(fileSizeCap)+1))
	if err != nil {
		return "", errors.New("file not found")
	}
	if len(content) > fileSizeCap {
		return "", errFileCollabTooLarge
	}
	if !utf8.Valid(content) {
		return "", errors.New("file is not editable as text")
	}
	return string(content), nil
}

func nativeFileCollabNewSession(serverUUID string, path string, content string) *nativeFileCollabSession {
	session := &nativeFileCollabSession{
		serverUUID: serverUUID,
		path:       path,
		members:    map[string]*nativeFileCollabMember{},
	}
	session.resetDocument(content)
	return session
}

func (session *nativeFileCollabSession) resetDocument(content string) {
	if session.doc != nil {
		session.doc.Destroy()
	}
	doc := crdt.New()
	text := doc.GetText("content")
	if content != "" {
		doc.Transact(func(transaction *crdt.Transaction) {
			text.Insert(transaction, 0, content, nil)
		})
	}
	session.doc = doc
	session.text = text
	session.diskHash = sha256.Sum256([]byte(content))
	session.appliedUpdateBytes = 0
}

func (session *nativeFileCollabSession) applyUpdate(update []byte, origin any) (bool, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := crdt.ApplyUpdateV1(session.doc, update, origin); err != nil {
		return false, errors.New("invalid update")
	}
	content := session.text.ToString()
	session.dirty = true
	session.appliedUpdateBytes += int64(len(update))
	fileSizeCap := NativeFileCollaborationFileSizeCap()
	needsResync := len(content) > fileSizeCap || session.appliedUpdateBytes > int64(fileSizeCap)*8
	if !needsResync {
		return false, nil
	}
	if len(content) > fileSizeCap {
		content = nativeFileCollabTruncateUTF8(content, fileSizeCap)
	}
	session.resetDocument(content)
	session.dirty = true
	return true, nil
}

func nativeFileCollabKey(serverUUID string, path string) string {
	return serverUUID + "\x00" + path
}

func nativeFileCollabSafeAvatar(avatar *string) *string {
	if avatar == nil {
		return nil
	}
	value := betterFilesCollabSafeImage(*avatar)
	if value == "" {
		return nil
	}
	return &value
}

func nativeFileCollabTruncateUTF8(content string, maxBytes int) string {
	if len(content) <= maxBytes {
		return content
	}
	content = content[:maxBytes]
	for !utf8.ValidString(content) {
		content = content[:len(content)-1]
	}
	return content
}
