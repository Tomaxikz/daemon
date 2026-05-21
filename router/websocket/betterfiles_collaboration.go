package websocket

import (
	"context"
	"encoding/json"
	pathpkg "path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gbrlsnchs/jwt/v3"
	"github.com/pterodactyl/wings/router/tokens"
)

const (
	BetterFilesCollabJoinEvent         = Event("betterfiles:collab:join")
	BetterFilesCollabLeaveEvent        = Event("betterfiles:collab:leave")
	BetterFilesCollabCloseEvent        = Event("betterfiles:collab:close")
	BetterFilesCollabFileOpenEvent     = Event("betterfiles:collab:file:open")
	BetterFilesCollabFileCloseEvent    = Event("betterfiles:collab:file:close")
	BetterFilesCollabPresenceEvent     = Event("betterfiles:collab:presence")
	BetterFilesCollabPatchEvent        = Event("betterfiles:collab:patch")
	BetterFilesCollabSnapshotEvent     = Event("betterfiles:collab:snapshot")
	BetterFilesCollabFileSavedEvent    = Event("betterfiles:collab:file:saved")
	BetterFilesCollabFileCheckEvent    = Event("betterfiles:collab:file:check")
	BetterFilesCollabExternalEditEvent = Event("betterfiles:collab:file:external-modified")

	betterFilesCollabJoinedEvent          = Event("betterfiles:collab:joined")
	betterFilesCollabClosedEvent          = Event("betterfiles:collab:closed")
	betterFilesCollabLeftEvent            = Event("betterfiles:collab:left")
	betterFilesCollabMembersEvent         = Event("betterfiles:collab:members")
	betterFilesCollabFileUsersEvent       = Event("betterfiles:collab:file:users")
	betterFilesCollabPresenceOut          = Event("betterfiles:collab:presence")
	betterFilesCollabPatchOut             = Event("betterfiles:collab:patch")
	betterFilesCollabSnapshotOut          = Event("betterfiles:collab:snapshot")
	betterFilesCollabFileSavedOut         = Event("betterfiles:collab:file:saved")
	betterFilesCollabFileCheckResultEvent = Event("betterfiles:collab:file:check:result")
	betterFilesCollabExternalEditOut      = Event("betterfiles:collab:file:external-modified")
	betterFilesCollabErrorEvent           = Event("betterfiles:collab:error")

	betterFilesCollabReadPermission  = "file.read-content"
	betterFilesCollabWritePermission = "file.update"
	betterFilesCollabMaxPayloadBytes = 10 * 1024 * 1024
	betterFilesCollabMaxMessageBytes = betterFilesCollabMaxPayloadBytes*2 + 8192
	betterFilesCollabMaxUsers        = 20
	betterFilesCollabMaxPatchChanges = 64
	betterFilesCollabMaxPathBytes    = 4096
	betterFilesCollabMaxCursorValue  = 1000000
)

type betterFilesCollabUser struct {
	UUID         string `json:"uuid"`
	Username     string `json:"username"`
	Image        string `json:"image"`
	ConnectionID string `json:"connection_id,omitempty"`
}

type betterFilesCollabSessionInfo struct {
	Code         string `json:"code"`
	ServerUUID   string `json:"server_uuid"`
	OwnerUUID   string `json:"owner_uuid"`
	Mode         string `json:"mode"`
	MaxUsers     int    `json:"max_users"`
	MaxFileBytes int    `json:"max_file_bytes"`
	Token        string `json:"socket_token"`
	ExpiresAt    int64  `json:"-"`
}

type betterFilesCollabToken struct {
	jwt.Payload
	ServerUUID   string `json:"server_uuid"`
	UserUUID     string `json:"user_uuid"`
	Enabled      bool   `json:"better_files_collaboration"`
	Code         string `json:"collaboration_code"`
	OwnerUUID    string `json:"collaboration_owner_uuid"`
	Mode         string `json:"collaboration_mode"`
	MaxUsers     int    `json:"collaboration_max_users"`
	MaxFileBytes int    `json:"collaboration_max_file_bytes"`
	UserCanEdit  bool   `json:"collaboration_user_can_edit"`
	ExpiresAt    int64  `json:"collaboration_expires_at"`
}

func (p *betterFilesCollabToken) GetPayload() *jwt.Payload {
	return &p.Payload
}

type betterFilesCollabJoinPayload struct {
	Session betterFilesCollabSessionInfo `json:"session"`
	User    betterFilesCollabUser        `json:"user"`
}

type betterFilesCollabFilePayload struct {
	Code string `json:"code"`
	Path string `json:"path"`
}

type betterFilesCollabCodePayload struct {
	Code string `json:"code"`
}

type betterFilesCollabPresencePayload struct {
	Code      string `json:"code"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	Selection string `json:"selection,omitempty"`
}

type betterFilesCollabSnapshotPayload struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Version int64  `json:"version"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

type betterFilesCollabTextEdit struct {
	RangeOffset int    `json:"rangeOffset"`
	RangeLength int    `json:"rangeLength"`
	Text        string `json:"text"`
	BaseHash    string `json:"baseHash,omitempty"`
	BaseLength  int    `json:"baseLength,omitempty"`
}

type betterFilesCollabPatchPayload struct {
	Code    string                      `json:"code"`
	Path    string                      `json:"path"`
	Changes []betterFilesCollabTextEdit `json:"changes"`
	Version int64                       `json:"version"`
	Line    int                         `json:"line,omitempty"`
	Column  int                         `json:"column,omitempty"`
}

type betterFilesCollabSavedPayload struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Version int64  `json:"version"`
}

type betterFilesCollabFileCheckPayload struct {
	Path      string `json:"path"`
	RequestID string `json:"request_id,omitempty"`
}

type betterFilesCollabExternalEditPayload struct {
	Path string                `json:"path"`
	User betterFilesCollabUser `json:"user"`
}

type betterFilesCollabFileConflict struct {
	Code  string                  `json:"code"`
	Mode  string                  `json:"mode"`
	Users []betterFilesCollabUser `json:"users"`
}

type betterFilesCollabMember struct {
	HandlerID   string                `json:"handler_id"`
	User        betterFilesCollabUser `json:"user"`
	FilePath    string                `json:"file_path,omitempty"`
	JoinedAt    time.Time             `json:"joined_at"`
	LastVersion int64                 `json:"-"`
	handler     *Handler
}

type betterFilesCollabSession struct {
	sync.RWMutex
	Code         string
	ServerUUID   string
	OwnerUUID   string
	Mode         string
	MaxUsers     int
	MaxFileBytes int
	ExpiresAt    time.Time
	Members      map[string]*betterFilesCollabMember
}

var (
	betterFilesCollabMu       sync.Mutex
	betterFilesCollabSessions = map[string]*betterFilesCollabSession{}
)

// HandleBetterFilesCollaboration handles Better Files live collaboration socket messages.
// Add this near the top of Handler.HandleInbound after TokenValid succeeds:
//
//     if handled, err := h.HandleBetterFilesCollaboration(ctx, m); handled {
//         return err
//     }
//
func (h *Handler) HandleBetterFilesCollaboration(ctx context.Context, m Message) (bool, error) {
	switch m.Event {
	case BetterFilesCollabJoinEvent:
		return true, h.betterFilesCollabJoin(ctx, m)
	case BetterFilesCollabLeaveEvent:
		h.betterFilesCollabLeaveAll()
		return true, nil
	case BetterFilesCollabCloseEvent:
		return true, h.betterFilesCollabClose(m)
	case BetterFilesCollabFileOpenEvent:
		return true, h.betterFilesCollabSetFile(m, true)
	case BetterFilesCollabFileCloseEvent:
		return true, h.betterFilesCollabSetFile(m, false)
	case BetterFilesCollabPresenceEvent:
		return true, h.betterFilesCollabPresence(m)
	case BetterFilesCollabPatchEvent:
		return true, h.betterFilesCollabPatch(m)
	case BetterFilesCollabSnapshotEvent:
		return true, h.betterFilesCollabSnapshot(m)
	case BetterFilesCollabFileSavedEvent:
		return true, h.betterFilesCollabFileSaved(m)
	case BetterFilesCollabFileCheckEvent:
		return true, h.betterFilesCollabFileCheck(m)
	case BetterFilesCollabExternalEditEvent:
		return true, h.betterFilesCollabExternalEdit(m)
	default:
		return false, nil
	}
}

func (h *Handler) betterFilesCollabJoin(ctx context.Context, m Message) error {
	if !h.betterFilesCollabCanRead() {
		return h.betterFilesCollabError("missing file read permission")
	}

	var payload betterFilesCollabJoinPayload
	if !betterFilesCollabDecode(m, &payload) {
		return h.betterFilesCollabError("invalid collaboration join payload")
	}

	jwt := h.GetJwt()
	verifiedSession, tokenUserUUID, ok := betterFilesCollabVerifiedSession(payload.Session)
	if !ok {
		return h.betterFilesCollabError("invalid or expired collaboration session token")
	}

	if jwt == nil || verifiedSession.ServerUUID != jwt.GetServerUuid() || tokenUserUUID != jwt.UserUUID {
		return h.betterFilesCollabError("collaboration session does not match this server")
	}
	if verifiedSession.Mode == "edit" && !h.betterFilesCollabCanWrite() {
		return h.betterFilesCollabError("missing file write permission")
	}

	code := betterFilesCollabCode(verifiedSession.Code)
	if code == "" {
		return h.betterFilesCollabError("invalid collaboration session code")
	}

	user := payload.User
	user.UUID = jwt.UserUUID
	user.Username = betterFilesCollabSafeText(user.Username, 48)
	user.Image = betterFilesCollabSafeImage(user.Image)
	user.ConnectionID = betterFilesCollabSafeText(user.ConnectionID, 80)
	if user.Username == "" {
		user.Username = "User"
	}
	if user.ConnectionID == "" {
		user.ConnectionID = h.Uuid().String()
	}

	h.betterFilesCollabLeaveAll()

	session := betterFilesCollabGetOrCreateSession(verifiedSession)
	if session.ServerUUID != verifiedSession.ServerUUID || session.Mode != verifiedSession.Mode {
		return h.betterFilesCollabError("collaboration session metadata changed")
	}

	session.Lock()
	if len(session.Members) >= session.MaxUsers {
		session.Unlock()
		return h.betterFilesCollabError("collaboration session is full")
	}
	session.Members[h.Uuid().String()] = &betterFilesCollabMember{
		HandlerID: h.Uuid().String(),
		User:      user,
		JoinedAt:  time.Now(),
		handler:   h,
	}
	session.Unlock()

	go func(handlerID string, sessionCode string) {
		<-ctx.Done()
		betterFilesCollabRemoveMember(sessionCode, handlerID)
	}(h.Uuid().String(), code)

	h.betterFilesCollabSend(betterFilesCollabJoinedEvent, map[string]any{
		"code": code,
		"user": user,
		"mode": session.Mode,
	})
	betterFilesCollabBroadcastMembers(session)
	betterFilesCollabBroadcastFileUsers(session)

	return nil
}

func (h *Handler) betterFilesCollabSetFile(m Message, open bool) error {
	var payload betterFilesCollabFilePayload
	if !betterFilesCollabDecode(m, &payload) {
		return h.betterFilesCollabError("invalid collaboration file payload")
	}

	session, member := betterFilesCollabMemberFor(h, payload.Code)
	if session == nil || member == nil {
		return h.betterFilesCollabError("not joined to this collaboration session")
	}

	path, ok := betterFilesCollabPath(payload.Path)
	if !ok {
		return h.betterFilesCollabError("file is not allowed in live collaboration")
	}
	session.Lock()
	if open {
		member.FilePath = path
	} else if member.FilePath == path {
		member.FilePath = ""
	}
	session.Unlock()

	betterFilesCollabBroadcastFileUsers(session)
	return nil
}

func (h *Handler) betterFilesCollabClose(m Message) error {
	var payload betterFilesCollabCodePayload
	if !betterFilesCollabDecode(m, &payload) {
		return h.betterFilesCollabError("invalid collaboration close payload")
	}

	session, member := betterFilesCollabMemberFor(h, payload.Code)
	if session == nil || member == nil {
		return h.betterFilesCollabError("not joined to this collaboration session")
	}
	if session.OwnerUUID != "" && member.User.UUID != session.OwnerUUID {
		return h.betterFilesCollabError("only the live session owner can close it")
	}

	code := session.Code
	betterFilesCollabBroadcast(session, h.Uuid().String(), betterFilesCollabClosedEvent, map[string]any{
		"code": code,
	})

	betterFilesCollabMu.Lock()
	delete(betterFilesCollabSessions, code)
	betterFilesCollabMu.Unlock()

	return nil
}

func (h *Handler) betterFilesCollabPresence(m Message) error {
	var payload betterFilesCollabPresencePayload
	if !betterFilesCollabDecode(m, &payload) {
		return h.betterFilesCollabError("invalid collaboration presence payload")
	}

	session, member := betterFilesCollabMemberFor(h, payload.Code)
	if session == nil || member == nil {
		return h.betterFilesCollabError("not joined to this collaboration session")
	}

	path, ok := betterFilesCollabPath(payload.Path)
	if !ok {
		return h.betterFilesCollabError("file is not allowed in live collaboration")
	}
	payload.Path = path
	payload.Line = betterFilesCollabClampCursor(payload.Line)
	payload.Column = betterFilesCollabClampCursor(payload.Column)
	if betterFilesCollabSetMemberPath(session, member, payload.Path) {
		betterFilesCollabBroadcastFileUsers(session)
	}

	betterFilesCollabBroadcast(session, h.Uuid().String(), betterFilesCollabPresenceOut, map[string]any{
		"code": payload.Code,
		"path": payload.Path,
		"line": payload.Line,
		"column": payload.Column,
		"selection": betterFilesCollabSafeText(payload.Selection, 120),
		"user": member.User,
	})
	return nil
}

func (h *Handler) betterFilesCollabPatch(m Message) error {
	if !h.betterFilesCollabCanWrite() {
		return h.betterFilesCollabError("missing file write permission")
	}

	var payload betterFilesCollabPatchPayload
	if !betterFilesCollabDecode(m, &payload) {
		return h.betterFilesCollabError("invalid collaboration patch payload")
	}
	session, member := betterFilesCollabMemberFor(h, payload.Code)
	if session == nil || member == nil {
		return h.betterFilesCollabError("not joined to this collaboration session")
	}
	if session.Mode != "edit" {
		return h.betterFilesCollabError("collaboration session is view-only")
	}
	if len(payload.Changes) == 0 || len(payload.Changes) > betterFilesCollabMaxPatchChanges {
		return h.betterFilesCollabError("invalid collaboration patch size")
	}

	totalTextBytes := 0
	for _, change := range payload.Changes {
		if change.RangeOffset < 0 || change.RangeLength < 0 || change.BaseLength < 0 || !utf8.ValidString(change.Text) {
			return h.betterFilesCollabError("invalid collaboration patch change")
		}
		if change.RangeOffset > betterFilesCollabMaxPayloadBytes || change.RangeLength > betterFilesCollabMaxPayloadBytes || change.BaseLength > betterFilesCollabMaxPayloadBytes {
			return h.betterFilesCollabError("invalid collaboration patch change")
		}
		totalTextBytes += len(change.Text)
	}

	maxPayloadBytes := session.MaxFileBytes
	if maxPayloadBytes <= 0 || maxPayloadBytes > betterFilesCollabMaxPayloadBytes {
		maxPayloadBytes = betterFilesCollabMaxPayloadBytes
	}
	if totalTextBytes > maxPayloadBytes {
		return h.betterFilesCollabError("collaboration patch is too large")
	}

	path, ok := betterFilesCollabPath(payload.Path)
	if !ok {
		return h.betterFilesCollabError("file is not allowed in live collaboration")
	}
	payload.Path = path
	payload.Line = betterFilesCollabClampCursor(payload.Line)
	payload.Column = betterFilesCollabClampCursor(payload.Column)
	if !betterFilesCollabAcceptVersion(session, member, payload.Version) {
		return nil
	}
	if betterFilesCollabSetMemberPath(session, member, payload.Path) {
		betterFilesCollabBroadcastFileUsers(session)
	}

	betterFilesCollabBroadcast(session, h.Uuid().String(), betterFilesCollabPatchOut, map[string]any{
		"code":    payload.Code,
		"path":    payload.Path,
		"changes": payload.Changes,
		"version": payload.Version,
		"line":    payload.Line,
		"column":  payload.Column,
		"user":    member.User,
	})
	return nil
}

func (h *Handler) betterFilesCollabSnapshot(m Message) error {
	if !h.betterFilesCollabCanWrite() {
		return h.betterFilesCollabError("missing file write permission")
	}

	var payload betterFilesCollabSnapshotPayload
	if !betterFilesCollabDecode(m, &payload) {
		return h.betterFilesCollabError("invalid collaboration snapshot payload")
	}
	session, member := betterFilesCollabMemberFor(h, payload.Code)
	if session == nil || member == nil {
		return h.betterFilesCollabError("not joined to this collaboration session")
	}
	if session.Mode != "edit" {
		return h.betterFilesCollabError("collaboration session is view-only")
	}
	maxPayloadBytes := session.MaxFileBytes
	if maxPayloadBytes <= 0 || maxPayloadBytes > betterFilesCollabMaxPayloadBytes {
		maxPayloadBytes = betterFilesCollabMaxPayloadBytes
	}
	if len(payload.Content) > maxPayloadBytes || !utf8.ValidString(payload.Content) {
		return h.betterFilesCollabError("collaboration snapshot is too large or invalid")
	}

	path, ok := betterFilesCollabPath(payload.Path)
	if !ok {
		return h.betterFilesCollabError("file is not allowed in live collaboration")
	}
	payload.Path = path
	payload.Line = betterFilesCollabClampCursor(payload.Line)
	payload.Column = betterFilesCollabClampCursor(payload.Column)
	if !betterFilesCollabAcceptVersion(session, member, payload.Version) {
		return nil
	}
	if betterFilesCollabSetMemberPath(session, member, payload.Path) {
		betterFilesCollabBroadcastFileUsers(session)
	}

	betterFilesCollabBroadcast(session, h.Uuid().String(), betterFilesCollabSnapshotOut, map[string]any{
		"code": payload.Code,
		"path": payload.Path,
		"content": payload.Content,
		"version": payload.Version,
		"line": payload.Line,
		"column": payload.Column,
		"user": member.User,
	})
	return nil
}

func (h *Handler) betterFilesCollabFileSaved(m Message) error {
	if !h.betterFilesCollabCanWrite() {
		return h.betterFilesCollabError("missing file write permission")
	}

	var payload betterFilesCollabSavedPayload
	if !betterFilesCollabDecode(m, &payload) {
		return h.betterFilesCollabError("invalid collaboration save payload")
	}

	session, member := betterFilesCollabMemberFor(h, payload.Code)
	if session == nil || member == nil {
		return h.betterFilesCollabError("not joined to this collaboration session")
	}
	if session.Mode != "edit" {
		return h.betterFilesCollabError("collaboration session is view-only")
	}

	maxPayloadBytes := session.MaxFileBytes
	if maxPayloadBytes <= 0 || maxPayloadBytes > betterFilesCollabMaxPayloadBytes {
		maxPayloadBytes = betterFilesCollabMaxPayloadBytes
	}
	if len(payload.Content) > maxPayloadBytes || !utf8.ValidString(payload.Content) {
		return h.betterFilesCollabError("collaboration save payload is too large or invalid")
	}

	path, ok := betterFilesCollabPath(payload.Path)
	if !ok {
		return h.betterFilesCollabError("file is not allowed in live collaboration")
	}
	payload.Path = path
	if !betterFilesCollabAcceptVersion(session, member, payload.Version) {
		return nil
	}
	if betterFilesCollabSetMemberPath(session, member, payload.Path) {
		betterFilesCollabBroadcastFileUsers(session)
	}

	betterFilesCollabBroadcast(session, h.Uuid().String(), betterFilesCollabFileSavedOut, map[string]any{
		"code": payload.Code,
		"path": payload.Path,
		"content": payload.Content,
		"version": payload.Version,
		"user": member.User,
	})
	return nil
}

func (h *Handler) betterFilesCollabFileCheck(m Message) error {
	if !h.betterFilesCollabCanRead() {
		return h.betterFilesCollabError("missing file read permission")
	}

	var payload betterFilesCollabFileCheckPayload
	if !betterFilesCollabDecode(m, &payload) {
		return h.betterFilesCollabError("invalid collaboration file check payload")
	}

	path, ok := betterFilesCollabPath(payload.Path)
	if !ok {
		return h.betterFilesCollabError("file is not allowed in live collaboration")
	}
	conflicts := betterFilesCollabConflictsForPath(h, path)
	users := make([]betterFilesCollabUser, 0)
	for _, conflict := range conflicts {
		users = append(users, conflict.Users...)
	}

	return h.betterFilesCollabSend(betterFilesCollabFileCheckResultEvent, map[string]any{
		"path": path,
		"request_id": betterFilesCollabSafeText(payload.RequestID, 80),
		"conflicts": conflicts,
		"users": users,
	})
}

func (h *Handler) betterFilesCollabExternalEdit(m Message) error {
	if !h.betterFilesCollabCanWrite() {
		return h.betterFilesCollabError("missing file write permission")
	}

	var payload betterFilesCollabExternalEditPayload
	if !betterFilesCollabDecode(m, &payload) {
		return h.betterFilesCollabError("invalid external edit payload")
	}

	jwt := h.GetJwt()
	if jwt == nil {
		return h.betterFilesCollabError("missing websocket identity")
	}

	path, ok := betterFilesCollabPath(payload.Path)
	if !ok {
		return h.betterFilesCollabError("file is not allowed in live collaboration")
	}
	user := payload.User
	user.UUID = jwt.UserUUID
	user.Username = betterFilesCollabSafeText(user.Username, 48)
	user.Image = betterFilesCollabSafeImage(user.Image)
	user.ConnectionID = betterFilesCollabSafeText(user.ConnectionID, 80)
	if user.Username == "" {
		user.Username = "User"
	}
	if user.ConnectionID == "" {
		user.ConnectionID = h.Uuid().String()
	}

	for _, session := range betterFilesCollabSessionsForServer(jwt.GetServerUuid()) {
		if session.Mode != "edit" {
			continue
		}

		users := betterFilesCollabUsersForPath(session, path, h.Uuid().String())
		if len(users) == 0 {
			continue
		}

		betterFilesCollabBroadcast(session, "", betterFilesCollabExternalEditOut, map[string]any{
			"code": session.Code,
			"path": path,
			"user": user,
			"editing_users": users,
		})
	}

	return nil
}

func (h *Handler) betterFilesCollabCanRead() bool {
	jwt := h.GetJwt()
	return jwt != nil && jwt.HasPermission(betterFilesCollabReadPermission)
}

func (h *Handler) betterFilesCollabCanWrite() bool {
	jwt := h.GetJwt()
	return jwt != nil && jwt.HasPermission(betterFilesCollabWritePermission)
}

func (h *Handler) betterFilesCollabLeaveAll() {
	betterFilesCollabMu.Lock()
	sessions := make([]*betterFilesCollabSession, 0, len(betterFilesCollabSessions))
	for _, session := range betterFilesCollabSessions {
		sessions = append(sessions, session)
	}
	betterFilesCollabMu.Unlock()

	for _, session := range sessions {
		betterFilesCollabRemoveMember(session.Code, h.Uuid().String())
	}
}

func (h *Handler) betterFilesCollabError(message string) error {
	return h.betterFilesCollabSend(betterFilesCollabErrorEvent, map[string]string{"error": message})
}

func (h *Handler) betterFilesCollabSend(event Event, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return h.SendJson(Message{Event: event, Args: []string{string(data)}})
}

func betterFilesCollabDecode(m Message, dst any) bool {
	if len(m.Args) == 0 || len(m.Args[0]) > betterFilesCollabMaxMessageBytes {
		return false
	}
	return json.Unmarshal([]byte(m.Args[0]), dst) == nil
}

func betterFilesCollabVerifiedSession(info betterFilesCollabSessionInfo) (betterFilesCollabSessionInfo, string, bool) {
	if strings.TrimSpace(info.Token) == "" {
		return betterFilesCollabSessionInfo{}, "", false
	}

	var token betterFilesCollabToken
	if err := tokens.ParseToken([]byte(info.Token), &token); err != nil {
		return betterFilesCollabSessionInfo{}, "", false
	}
	if !token.Enabled {
		return betterFilesCollabSessionInfo{}, "", false
	}

	code := betterFilesCollabCode(token.Code)
	if code == "" || code != betterFilesCollabCode(info.Code) {
		return betterFilesCollabSessionInfo{}, "", false
	}

	mode := token.Mode
	if mode != "edit" {
		mode = "view"
	}
	maxUsers := token.MaxUsers
	if maxUsers < 2 || maxUsers > betterFilesCollabMaxUsers {
		maxUsers = 10
	}

	maxFileBytes := token.MaxFileBytes
	if maxFileBytes < 1 || maxFileBytes > betterFilesCollabMaxPayloadBytes {
		maxFileBytes = betterFilesCollabMaxPayloadBytes
	}
	expiresAt := token.ExpiresAt
	if expiresAt <= 0 {
		expiresAt = time.Now().Add(15 * time.Minute).Unix()
	}
	if expiresAt <= time.Now().Unix() {
		return betterFilesCollabSessionInfo{}, "", false
	}

	return betterFilesCollabSessionInfo{
		Code:         code,
		ServerUUID:   token.ServerUUID,
		OwnerUUID:   token.OwnerUUID,
		Mode:         mode,
		MaxUsers:     maxUsers,
		MaxFileBytes: maxFileBytes,
		Token:        info.Token,
		ExpiresAt:    expiresAt,
	}, token.UserUUID, true
}

func betterFilesCollabGetOrCreateSession(info betterFilesCollabSessionInfo) *betterFilesCollabSession {
	code := betterFilesCollabCode(info.Code)
	betterFilesCollabMu.Lock()
	defer betterFilesCollabMu.Unlock()

	if session, ok := betterFilesCollabSessions[code]; ok {
		if betterFilesCollabSessionExpired(session) {
			delete(betterFilesCollabSessions, code)
		} else {
			session.Lock()
			if info.ExpiresAt > 0 && session.ExpiresAt.Before(time.Unix(info.ExpiresAt, 0)) {
				session.ExpiresAt = time.Unix(info.ExpiresAt, 0)
			}
			session.Unlock()
			return session
		}
	}

	mode := info.Mode
	if mode != "edit" {
		mode = "view"
	}
	maxUsers := info.MaxUsers
	if maxUsers < 2 || maxUsers > betterFilesCollabMaxUsers {
		maxUsers = 10
	}
	maxFileBytes := info.MaxFileBytes
	if maxFileBytes < 1 || maxFileBytes > betterFilesCollabMaxPayloadBytes {
		maxFileBytes = betterFilesCollabMaxPayloadBytes
	}

	session := &betterFilesCollabSession{
		Code:         code,
		ServerUUID:   info.ServerUUID,
		OwnerUUID:   info.OwnerUUID,
		Mode:         mode,
		MaxUsers:     maxUsers,
		MaxFileBytes: maxFileBytes,
		ExpiresAt:    time.Unix(info.ExpiresAt, 0),
		Members:      map[string]*betterFilesCollabMember{},
	}
	betterFilesCollabSessions[code] = session

	return session
}

func betterFilesCollabMemberFor(h *Handler, code string) (*betterFilesCollabSession, *betterFilesCollabMember) {
	code = betterFilesCollabCode(code)
	betterFilesCollabMu.Lock()
	session := betterFilesCollabSessions[code]
	betterFilesCollabMu.Unlock()
	if session == nil {
		return nil, nil
	}
	if betterFilesCollabSessionExpired(session) {
		betterFilesCollabMu.Lock()
		if betterFilesCollabSessions[code] == session {
			delete(betterFilesCollabSessions, code)
		}
		betterFilesCollabMu.Unlock()
		return nil, nil
	}

	session.RLock()
	member := session.Members[h.Uuid().String()]
	session.RUnlock()
	return session, member
}

func betterFilesCollabRemoveMember(code string, handlerID string) {
	code = betterFilesCollabCode(code)
	betterFilesCollabMu.Lock()
	session := betterFilesCollabSessions[code]
	betterFilesCollabMu.Unlock()
	if session == nil {
		return
	}

	session.Lock()
	if _, ok := session.Members[handlerID]; !ok {
		session.Unlock()
		return
	}
	delete(session.Members, handlerID)
	empty := len(session.Members) == 0
	session.Unlock()

	if empty {
		betterFilesCollabMu.Lock()
		delete(betterFilesCollabSessions, code)
		betterFilesCollabMu.Unlock()
		return
	}

	betterFilesCollabBroadcastMembers(session)
	betterFilesCollabBroadcastFileUsers(session)
}

func betterFilesCollabAcceptVersion(session *betterFilesCollabSession, member *betterFilesCollabMember, version int64) bool {
	if version < 1 {
		return false
	}

	session.Lock()
	defer session.Unlock()

	if version <= member.LastVersion {
		return false
	}

	member.LastVersion = version
	return true
}

func betterFilesCollabSetMemberPath(session *betterFilesCollabSession, member *betterFilesCollabMember, path string) bool {
	session.Lock()
	defer session.Unlock()

	if member.FilePath == path {
		return false
	}

	member.FilePath = path
	return true
}

func betterFilesCollabSessionsForServer(serverUUID string) []*betterFilesCollabSession {
	betterFilesCollabMu.Lock()
	sessions := make([]*betterFilesCollabSession, 0, len(betterFilesCollabSessions))
	for _, session := range betterFilesCollabSessions {
		if betterFilesCollabSessionExpired(session) {
			delete(betterFilesCollabSessions, session.Code)
			continue
		}
		if session.ServerUUID == serverUUID {
			sessions = append(sessions, session)
		}
	}
	betterFilesCollabMu.Unlock()

	return sessions
}

// CloseBetterFilesCollaborationSessions closes active in-memory Better Files
// collaboration sessions for a server. It is called by the protected Wings API
// when panel settings revoke collaboration access.
func CloseBetterFilesCollaborationSessions(serverUUID string, mode string, reason string) int {
	mode = strings.TrimSpace(mode)
	if mode != "edit" && mode != "view" {
		mode = ""
	}
	reason = betterFilesCollabSafeText(reason, 160)
	if reason == "" {
		reason = "Live collaboration was revoked by an administrator."
	}

	betterFilesCollabMu.Lock()
	sessions := make([]*betterFilesCollabSession, 0, len(betterFilesCollabSessions))
	for code, session := range betterFilesCollabSessions {
		if betterFilesCollabSessionExpired(session) {
			delete(betterFilesCollabSessions, code)
			continue
		}
		if session.ServerUUID != serverUUID {
			continue
		}
		if mode != "" && session.Mode != mode {
			continue
		}

		delete(betterFilesCollabSessions, code)
		sessions = append(sessions, session)
	}
	betterFilesCollabMu.Unlock()

	for _, session := range sessions {
		betterFilesCollabBroadcast(session, "", betterFilesCollabClosedEvent, map[string]any{
			"code":   session.Code,
			"reason": reason,
		})

		session.Lock()
		session.Members = map[string]*betterFilesCollabMember{}
		session.Unlock()
	}

	return len(sessions)
}

func betterFilesCollabUsersForPath(session *betterFilesCollabSession, path string, exceptHandlerID string) []betterFilesCollabUser {
	users := make([]betterFilesCollabUser, 0)

	session.RLock()
	for _, member := range session.Members {
		if exceptHandlerID != "" && member.HandlerID == exceptHandlerID {
			continue
		}
		if member.FilePath == path {
			users = append(users, member.User)
		}
	}
	session.RUnlock()

	return users
}

func betterFilesCollabConflictsForPath(h *Handler, path string) []betterFilesCollabFileConflict {
	jwt := h.GetJwt()
	if jwt == nil {
		return nil
	}

	conflicts := make([]betterFilesCollabFileConflict, 0)
	for _, session := range betterFilesCollabSessionsForServer(jwt.GetServerUuid()) {
		if session.Mode != "edit" {
			continue
		}

		users := betterFilesCollabUsersForPath(session, path, h.Uuid().String())
		if len(users) == 0 {
			continue
		}

		conflicts = append(conflicts, betterFilesCollabFileConflict{
			Code:  session.Code,
			Mode:  session.Mode,
			Users: users,
		})
	}

	return conflicts
}

func betterFilesCollabBroadcastMembers(session *betterFilesCollabSession) {
	members := betterFilesCollabMembers(session)
	betterFilesCollabBroadcast(session, "", betterFilesCollabMembersEvent, map[string]any{
		"code": session.Code,
		"members": members,
		"mode": session.Mode,
	})
}

func betterFilesCollabBroadcastFileUsers(session *betterFilesCollabSession) {
	fileUsers := map[string][]betterFilesCollabUser{}

	session.RLock()
	for _, member := range session.Members {
		if member.FilePath == "" {
			continue
		}
		fileUsers[member.FilePath] = append(fileUsers[member.FilePath], member.User)
	}
	session.RUnlock()

	betterFilesCollabBroadcast(session, "", betterFilesCollabFileUsersEvent, map[string]any{
		"code": session.Code,
		"files": fileUsers,
	})
}

func betterFilesCollabMembers(session *betterFilesCollabSession) []betterFilesCollabUser {
	session.RLock()
	defer session.RUnlock()

	members := make([]betterFilesCollabUser, 0, len(session.Members))
	for _, member := range session.Members {
		members = append(members, member.User)
	}
	return members
}

func betterFilesCollabBroadcast(session *betterFilesCollabSession, exceptHandlerID string, event Event, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg := Message{Event: event, Args: []string{string(data)}}

	session.RLock()
	members := make([]*betterFilesCollabMember, 0, len(session.Members))
	for _, member := range session.Members {
		if exceptHandlerID != "" && member.HandlerID == exceptHandlerID {
			continue
		}
		members = append(members, member)
	}
	session.RUnlock()

	for _, member := range members {
		_ = member.handler.SendJson(msg)
	}
}

func betterFilesCollabCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, " ", "")
	if len(code) == 8 {
		code = code[:4] + "-" + code[4:]
	}
	if len(code) != 9 || code[4] != '-' {
		return ""
	}
	for i, r := range code {
		if i == 4 {
			continue
		}
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return code
}

func betterFilesCollabSessionExpired(session *betterFilesCollabSession) bool {
	session.RLock()
	expiresAt := session.ExpiresAt
	session.RUnlock()

	return expiresAt.IsZero() || !expiresAt.After(time.Now())
}

func betterFilesCollabPath(path string) (string, bool) {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	if path == "" || len(path) > betterFilesCollabMaxPathBytes || strings.Contains(path, "\x00") || !utf8.ValidString(path) {
		return "", false
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return "", false
		}
	}
	path = pathpkg.Clean(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if path == "." || path == "/" || len(path) > betterFilesCollabMaxPathBytes || betterFilesCollabSensitivePath(path) {
		return "", false
	}
	return path, true
}

func betterFilesCollabSensitivePath(path string) bool {
	parts := strings.Split(strings.ToLower(strings.Trim(path, "/")), "/")
	for _, part := range parts {
		if part == "" {
			continue
		}
		if part == ".env" || strings.HasPrefix(part, ".env.") || part == ".npmrc" || part == "composer.auth.json" {
			return true
		}
		if part == "id_rsa" || part == "id_ed25519" || part == "id_ecdsa" {
			return true
		}
		if strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") || strings.HasSuffix(part, ".p12") || strings.HasSuffix(part, ".pfx") {
			return true
		}
		if strings.HasPrefix(part, "secret") || strings.HasPrefix(part, "credential") {
			return true
		}
	}
	return false
}

func betterFilesCollabClampCursor(value int) int {
	if value < 1 {
		return 1
	}
	if value > betterFilesCollabMaxCursorValue {
		return betterFilesCollabMaxCursorValue
	}
	return value
}

func betterFilesCollabSafeImage(value string) string {
	value = betterFilesCollabSafeText(value, 240)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "https://") {
		return value
	}
	return ""
}

func betterFilesCollabSafeText(value string, max int) string {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) {
		return ""
	}
	if len(value) > max {
		runes := []rune(value)
		if len(runes) > max {
			return string(runes[:max])
		}
	}
	return value
}
