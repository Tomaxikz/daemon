package websocket

import (
	"context"
	"encoding/json"
	"errors"
)

// HandleServerImporter handles lightweight importer socket requests from the panel.
func (h *Handler) HandleServerImporter(ctx context.Context, m Message) (bool, error) {
	switch m.Event {
	case ServerImporterProgressGetEvent:
		jwt := h.GetJwt()
		if jwt == nil || !jwt.HasPermission(PermissionReceiveImporter) {
			return true, h.SendErrorJson(m, errors.New("missing importer status permission"), false)
		}
		if err := ctx.Err(); err != nil {
			return true, err
		}
		return true, h.sendServerImporterProgress()
	default:
		return false, nil
	}
}

func (h *Handler) sendServerImporterProgress() error {
	progress, ok := h.server.GetImportProgressSnapshot()
	payload := map[string]interface{}{
		"is_importing": h.server.IsImporting(),
		"progress":     nil,
	}
	if ok {
		payload["progress"] = progress
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	return h.SendJson(Message{Event: serverImporterProgressEvent, Args: []string{string(body)}})
}
