package docker

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/buger/jsonparser"
)

func betterConsoleDockerPullProgress(img string, b []byte) string {
	id, _ := jsonparser.GetString(b, "id")
	status, _ := jsonparser.GetString(b, "status")
	progress, _ := jsonparser.GetString(b, "progress")
	current, currentErr := jsonparser.GetInt(b, "progressDetail", "current")
	total, totalErr := jsonparser.GetInt(b, "progressDetail", "total")
	status = strings.TrimSpace(status)
	if status == "" && id == "" {
		return ""
	}

	displayStatus, phase := betterConsoleDockerPullStatus(status)
	hasProgressDetail := currentErr == nil && totalErr == nil
	percent := betterConsoleDockerPullPercent(current, total, phase == "complete")
	line := betterConsoleDockerPullLine(displayStatus, current, total, hasProgressDetail)
	image := betterConsoleDockerPullSafeImageRef(img)

	payload, err := json.Marshal(map[string]interface{}{
		"id":       betterConsoleDockerPullSafeText(id),
		"image":    image,
		"label":    betterConsoleDockerPullImageLabel(image),
		"phase":    phase,
		"status":   displayStatus,
		"progress": betterConsoleDockerPullSafeText(progress),
		"current":  current,
		"total":    total,
		"percent":  percent,
		"line":     line,
	})
	if err != nil {
		return line
	}

	return string(payload)
}

func betterConsoleDockerPullStatus(status string) (string, string) {
	normalized := strings.ToLower(strings.TrimSpace(status))
	switch normalized {
	case "downloading":
		return "Downloading", "pulling"
	case "extracting":
		return "Extracting", "extracting"
	case "download complete":
		return "Download complete", "complete"
	case "pull complete":
		return "Pull complete", "complete"
	case "already exists":
		return "Already exists", "complete"
	default:
		if status == "" {
			return "Pulling", "pulling"
		}
		return betterConsoleDockerPullSafeText(status), "status"
	}
}

func betterConsoleDockerPullPercent(current int64, total int64, complete bool) float64 {
	if complete {
		return 100
	}
	if total <= 0 || current <= 0 {
		return 0
	}

	percent := (float64(current) / float64(total)) * 100
	if percent > 100 {
		return 100
	}
	if percent < 0 {
		return 0
	}

	return percent
}

func betterConsoleDockerPullLine(status string, current int64, total int64, complete bool) string {
	if !complete {
		return strings.TrimSpace(status)
	}

	percent := betterConsoleDockerPullPercent(current, total, false)
	width := 50 - len(status)
	if width < 8 {
		width = 8
	}

	return fmt.Sprintf(
		"%s %s %.2f%% of %s",
		status,
		betterConsoleDockerPullBar(width, percent),
		percent,
		betterConsoleDockerPullBytes(total),
	)
}

func betterConsoleDockerPullBar(width int, percent float64) string {
	completed := int(math.Round((percent / 100) * float64(width)))
	if completed >= width {
		return "[" + strings.Repeat("=", width) + "]"
	}
	if completed < 0 {
		completed = 0
	}

	return "[" + strings.Repeat("=", completed) + ">" + strings.Repeat(" ", width-completed-1) + "]"
}

func betterConsoleDockerPullBytes(value int64) string {
	if value <= 0 {
		return "0 B"
	}

	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	size := float64(value)
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}

	if unit == 0 {
		return fmt.Sprintf("%d B", value)
	}

	return fmt.Sprintf("%.1f %s", size, units[unit])
}

func betterConsoleDockerPullImageLabel(img string) string {
	parts := strings.Split(strings.TrimSpace(img), "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return "Docker image"
	}

	return parts[len(parts)-1]
}

func betterConsoleDockerPullSafeImageRef(img string) string {
	img = strings.TrimSpace(img)
	slash := strings.IndexByte(img, '/')
	if slash <= 0 {
		return img
	}

	registry := img[:slash]
	if at := strings.LastIndex(registry, "@"); at >= 0 {
		registry = registry[at+1:]
	}

	return registry + "/" + img[slash+1:]
}

func betterConsoleDockerPullSafeText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r >= 32 {
			return r
		}
		return -1
	}, strings.TrimSpace(value))
}
