package docker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBetterConsoleDockerPullProgressWithProgressDetail(t *testing.T) {
	payload := betterConsoleDockerPullProgress("ghcr.io/ptero-eggs/yolks:nodejs_22", []byte(`{"id":"layer","status":"Downloading","progressDetail":{"current":512,"total":1024}}`))

	var parsed struct {
		Line    string  `json:"line"`
		Percent float64 `json:"percent"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Percent != 50 {
		t.Fatalf("expected 50 percent, got %f", parsed.Percent)
	}
	if !strings.HasPrefix(parsed.Line, "Downloading [") {
		t.Fatalf("expected progress bar line, got %q", parsed.Line)
	}
}

func TestBetterConsoleDockerPullProgressWithoutProgressDetail(t *testing.T) {
	payload := betterConsoleDockerPullProgress("ghcr.io/ptero-eggs/yolks:nodejs_22", []byte(`{"status":"Status: Image is up to date for ghcr.io/ptero-eggs/yolks:nodejs_22"}`))

	var parsed struct {
		Line string `json:"line"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(parsed.Line, "0 B") || strings.Contains(parsed.Line, "[") {
		t.Fatalf("expected plain status without fake bar, got %q", parsed.Line)
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
