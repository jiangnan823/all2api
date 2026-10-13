package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"all2api/internal/adapter"
)

// Consent trail: append-only JSONL next to the configured state file
// (default ./data/consent_trail.jsonl). Each line is exactly the three
// UI §5.19 留痕 fields — source + notice_version + server-stamped ack_at.
// It never contains account identifiers, credential content, or notice
// text, and is never uploaded (PRD §6⑥).

func consentTrailPath(stateFile string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "consent_trail.jsonl")
}

func appendConsentTrail(stateFile string, t adapter.ConsentTrail) error {
	path := consentTrailPath(stateFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(t)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

const localLayout = "2006-01-02 15:04:05"

func nowLocal() adapter.TimeLocal {
	now := time.Now()
	return adapter.TimeLocal{Unix: now.Unix(), Local: now.Format(localLayout)}
}

func unixLocal(unix int64) *adapter.TimeLocal {
	t := time.Unix(unix, 0)
	return &adapter.TimeLocal{Unix: unix, Local: t.Format(localLayout)}
}
