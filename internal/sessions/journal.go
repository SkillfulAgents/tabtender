package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/config"
)

// Record is the durable trace of a running session. It carries exactly what
// reconciliation needs to find and validate the browser after a daemon
// restart.
type Record struct {
	ID         string    `json:"id"`
	PID        int       `json:"pid"`
	ProfileDir string    `json:"profileDir"`
	Ephemeral  bool      `json:"ephemeral"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Journal persists session records to a JSON file.
type Journal struct {
	path string
}

// NewJournal returns a journal stored at path.
func NewJournal(path string) *Journal { return &Journal{path: path} }

// Load returns the recorded sessions; a missing journal is empty.
func (j *Journal) Load() ([]Record, error) {
	data, err := os.ReadFile(j.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session journal: %w", err)
	}
	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("parse session journal: %w", err)
	}
	return records, nil
}

// Save atomically replaces the journal contents.
func (j *Journal) Save(records []Record) error {
	if records == nil {
		records = []Record{}
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(j.path, data, 0o600)
}
