package websocket

import (
	"strings"

	"github.com/pterodactyl/wings/server"
)

const (
	ServerImporterProgressGetEvent = Event("serverimporter:progress:get")
	serverImporterProgressEvent    = Event(server.ServerImporterProgressEvent)
	serverImporterCompletedEvent   = Event(server.ServerImporterCompletedEvent)
)

func isServerImporterEvent(e Event) bool {
	return strings.HasPrefix(string(e), "serverimporter:")
}
