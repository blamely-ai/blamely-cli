// Package authorship implements Attribution (see docs/attribution-v2-design.md):
// per-line authorship is determined by DIFFING a pre-edit baseline against the
// post-edit content and maintained in a stateful "working log" — never by
// content-hash guessing. This package is the single, tool-agnostic engine; every
// tool/editor path funnels through Attribute (attribute.go) and the working log
// is persisted as plain files (store.go), so correctness never depends on a
// running daemon or a database.
//
// Cross-platform: this package uses only path/filepath and portable file ops; it
// must behave identically on Windows, Linux, and macOS.
package authorship

import "encoding/json"

// WorkingLogSchema is the on-disk format version. It is also the shared contract
// implemented by the editor plugins (TypeScript, Kotlin); keep it and the JSON
// field tags below in lockstep across all three implementations.
const WorkingLogSchema = "blamely/working-log/1"

// AuthorType is the coarse AI-vs-Human split shown in the gutter and the bar.
type AuthorType string

const (
	Human AuthorType = "human"
	AI    AuthorType = "ai"
)

// Author identifies who wrote a line. For Human, only Type is set. For AI, Tool
// is required and the rest are best-effort metadata.
type Author struct {
	Type    AuthorType `json:"author"`
	Tool    string     `json:"tool,omitempty"`     // "claude" | "cursor" | "codex" | "copilot" | "gemini" | "devin" | ...
	Model   string     `json:"model,omitempty"`    // e.g. "claude-opus-4-8"
	GenType string     `json:"gen_type,omitempty"` // "chat" | "cli" | "completion" | "human"
	Session string     `json:"session,omitempty"`  // session/conversation id, if known
}

// HumanAuthor is the canonical Human author (no tool metadata).
func HumanAuthor() Author { return Author{Type: Human, GenType: "human"} }

// IsAI reports whether this author is an AI tool.
func (a Author) IsAI() bool { return a.Type == AI }

// equalForCoalesce decides whether two adjacent lines can merge into one range.
// We merge only when the full author identity matches, so a range never blurs two
// different tools/models/sessions together.
func (a Author) equalForCoalesce(b Author) bool { return a == b }

// LineAttribution is a contiguous, 1-based, inclusive run of lines by one author.
// Overrode, when set, records the author this run replaced (a human rewriting AI
// code, or vice-versa) — used by reporting and by later transform rules.
type LineAttribution struct {
	Start    int     `json:"start"`
	End      int     `json:"end"`
	Author   Author  `json:"-"` // flattened into the JSON object via MarshalJSON
	Overrode *Author `json:"overrode,omitempty"`
	// CharRanges is reserved for sub-line attribution (Decision A); not yet implemented.
}

// LineAttribution serializes as a flat object: {start,end,author,tool,...} so the
// format reads naturally and matches docs/attribution-v2-design.md §6.
func (l LineAttribution) MarshalJSON() ([]byte, error) {
	type wire struct {
		Start    int        `json:"start"`
		End      int        `json:"end"`
		Author   AuthorType `json:"author"`
		Tool     string     `json:"tool,omitempty"`
		Model    string     `json:"model,omitempty"`
		GenType  string     `json:"gen_type,omitempty"`
		Session  string     `json:"session,omitempty"`
		Overrode *Author    `json:"overrode,omitempty"`
	}
	return json.Marshal(wire{
		Start: l.Start, End: l.End,
		Author: l.Author.Type, Tool: l.Author.Tool, Model: l.Author.Model,
		GenType: l.Author.GenType, Session: l.Author.Session,
		Overrode: l.Overrode,
	})
}

func (l *LineAttribution) UnmarshalJSON(b []byte) error {
	var w struct {
		Start    int        `json:"start"`
		End      int        `json:"end"`
		Author   AuthorType `json:"author"`
		Tool     string     `json:"tool"`
		Model    string     `json:"model"`
		GenType  string     `json:"gen_type"`
		Session  string     `json:"session"`
		Overrode *Author    `json:"overrode"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	l.Start, l.End = w.Start, w.End
	l.Author = Author{Type: w.Author, Tool: w.Tool, Model: w.Model, GenType: w.GenType, Session: w.Session}
	l.Overrode = w.Overrode
	return nil
}

// WorkingLog is the per-file, uncommitted line-authorship state. It is keyed on
// disk by repo+branch+base_sha+file (see store.go); BlobSHA gates staleness.
type WorkingLog struct {
	Schema    string            `json:"schema"`
	File      string            `json:"file"`     // repo-relative, forward-slashed; never parsed from a diff header
	BaseSHA   string            `json:"base_sha"` // commit the baseline is relative to
	BlobSHA   string            `json:"blob_sha"` // sha256 of the content these attributions describe
	UpdatedMS int64             `json:"updated_ms"`
	Lines     []LineAttribution `json:"lines"`
}

// authorAtLine returns the author of 1-based line n, or Human if uncovered.
func (w *WorkingLog) authorAtLine(n int) Author {
	if w != nil {
		for _, r := range w.Lines {
			if n >= r.Start && n <= r.End {
				return r.Author
			}
		}
	}
	return HumanAuthor()
}

// coalesce turns a per-line author slice (index 0 = line 1) into contiguous
// ranges, merging adjacent lines only when the full author identity AND the
// overrode marker match (so an overriding edit stays a distinct run).
func coalesce(perLine []Author, overrode []*Author) []LineAttribution {
	var out []LineAttribution
	for i, a := range perLine {
		ln := i + 1
		ov := overrode[i]
		if n := len(out); n > 0 && out[n-1].End == ln-1 &&
			out[n-1].Author.equalForCoalesce(a) && overrideEqual(out[n-1].Overrode, ov) {
			out[n-1].End = ln
			continue
		}
		out = append(out, LineAttribution{Start: ln, End: ln, Author: a, Overrode: ov})
	}
	return out
}

// overrideEqual compares two overrode markers (nil = no override).
func overrideEqual(a, b *Author) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
