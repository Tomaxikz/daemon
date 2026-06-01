package websocket

import "github.com/pterodactyl/wings/server"

var serverImporterListenerEvents = []string{
	server.ServerImporterProgressEvent,
	server.ServerImporterCompletedEvent,
}
