package websocket

import "github.com/pterodactyl/wings/server"

var betterConsoleListenerEvents = []string{
	server.ImagePullStartedEvent,
	server.ImagePullProgressEvent,
	server.ImagePullCompletedEvent,
}

func isBetterConsoleInstallOutputEvent(event Event) bool {
	return event == server.InstallOutputEvent ||
		event == server.ImagePullStartedEvent ||
		event == server.ImagePullProgressEvent ||
		event == server.ImagePullCompletedEvent
}
