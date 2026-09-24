package autopilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LogsDir returns the ignored, local log-capture directory for a Config.
func LogsDir(cfg Config) string {
	return filepath.Join(cfg.RepoRoot, cfg.StateDir, "logs")
}

// WriteLog writes content under LogsDir in a category subdirectory (e.g.
// "gates", "ci", "claude"), named with a timestamp and label so repeated
// runs never overwrite prior evidence, and returns the path written.
func WriteLog(cfg Config, category, label, content string) (string, error) {
	dir := filepath.Join(LogsDir(cfg), category)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating log dir %s: %w", dir, err)
	}
	safeLabel := sanitizeLogLabel(label)
	name := fmt.Sprintf("%s-%s.log", time.Now().UTC().Format("20060102T150405Z"), safeLabel)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("writing log %s: %w", path, err)
	}
	return path, nil
}

func sanitizeLogLabel(label string) string {
	label = strings.ToLower(strings.TrimSpace(label))
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "log"
	}
	return b.String()
}
