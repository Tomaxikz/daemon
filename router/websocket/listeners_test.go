package websocket

import (
	"testing"

	"github.com/pterodactyl/wings/server"
)

func TestAllowedServerEvents(t *testing.T) {
	if _, ok := allowedServerEvents[server.ImagePullProgressEvent]; !ok {
		t.Fatalf("expected image pull progress event to be allowed")
	}
	if _, ok := allowedServerEvents["unexpected internal event"]; ok {
		t.Fatalf("expected unknown internal event to be blocked")
	}
}
