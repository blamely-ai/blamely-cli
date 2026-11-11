// Package hookdrift keeps a local tally of tool-hook payloads Blamely could not
// read precisely, so `blamely doctor` can say so.
//
// AI tools change the shape of their hook payloads between versions without
// notice. When that happens Blamely falls back to observing the disk (which still
// credits the edit), but the precise path is lost and nobody would know. This file
// is the canary: no telemetry leaves the machine, yet the next `blamely doctor`
// names the tool, the tool-call name and the payload's top-level KEYS — never its
// values, which can hold source code or prompts.
package hookdrift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/blamely/blamely/internal/config"
)

// Entry is the tally for one AI tool.
type Entry struct {
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// LastToolName is the tool-call name of the most recent unreadable payload
	// ("" when the payload named none).
	LastToolName string `json:"last_tool_name,omitempty"`
	// LastKeys is that payload's top-level key set.
	LastKeys []string `json:"last_keys,omitempty"`
}

const fileName = "hook_drift.json"

func path() (string, error) {
	dir, err := config.BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fileName), nil
}

// Note records one unreadable payload for tool. Best-effort: a hook must never
// fail because this bookkeeping did. Concurrent hooks can race and lose a count;
// the tally is a signal, not a ledger.
func Note(tool, toolName string, keys []string) {
	p, err := path()
	if err != nil {
		return
	}
	all := read(p)
	e := all[tool]
	now := time.Now().UTC()
	if e.Count == 0 {
		e.FirstSeen = now
	}
	e.Count++
	e.LastSeen = now
	e.LastToolName = toolName
	e.LastKeys = append([]string(nil), keys...)
	sort.Strings(e.LastKeys)
	all[tool] = e

	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), fileName+".*.tmp")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		_ = os.Remove(tmp.Name())
	}
}

// Read returns the tally per tool (empty when nothing was ever noted).
func Read() map[string]Entry {
	p, err := path()
	if err != nil {
		return map[string]Entry{}
	}
	return read(p)
}

// Reset clears the tally, e.g. after an update that taught Blamely the new shape.
func Reset() error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func read(p string) map[string]Entry {
	out := map[string]Entry{}
	data, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	if out == nil {
		out = map[string]Entry{}
	}
	return out
}
