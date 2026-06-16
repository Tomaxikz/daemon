package server

import (
	"strings"
	"testing"
)

func TestBetterConsoleDockerPullInstallLineWithProgressDetail(t *testing.T) {
	line := betterConsoleDockerPullInstallLine([]byte(`{"status":"Downloading","progressDetail":{"current":512,"total":1024}}`))

	if !strings.HasPrefix(line, "Downloading [") {
		t.Fatalf("expected downloading progress bar, got %q", line)
	}
	if !strings.Contains(line, "50.00% of 1.0 KiB") {
		t.Fatalf("expected calculated percentage and total bytes, got %q", line)
	}
}

func TestBetterConsoleDockerPullInstallLineWithoutProgressDetail(t *testing.T) {
	line := betterConsoleDockerPullInstallLine([]byte(`{"status":"Pull complete"}`))

	if line != "Pull complete" {
		t.Fatalf("expected plain status without fake 0 B bar, got %q", line)
	}
}

func TestBetterConsoleDockerPullProgressSanitizesImageCredentials(t *testing.T) {
	payload := betterConsoleDockerPullProgress("user:secret@registry.example.com/private/image:latest", []byte(`{"id":"layer","status":"Downloading","progressDetail":{"current":512,"total":1024}}`))

	if strings.Contains(payload, "secret") || strings.Contains(payload, "user:") {
		t.Fatalf("expected sanitized image payload, got %q", payload)
	}
	if !strings.Contains(payload, "registry.example.com/private/image:latest") {
		t.Fatalf("expected sanitized image reference in payload, got %q", payload)
	}
}

func TestBetterConsoleDockerPullProgressStripsControlCharacters(t *testing.T) {
	payload := betterConsoleDockerPullProgress("ghcr.io/ptero-eggs/yolks:nodejs_22", []byte("{\"id\":\"lay\u001ber\",\"status\":\"Pull\u001b complete\",\"progress\":\"10\u001b%\"}"))

	if strings.Contains(payload, "\u001b") {
		t.Fatalf("expected control characters to be stripped, got %q", payload)
	}
}
