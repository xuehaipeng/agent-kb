package extract

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuehaipeng/agent-kb/internal/kb"
)

// Compaction summaries as a capture source (opt-in).
//
// When Claude Code compacts a conversation it has the whole of it in
// context and writes a summary. That summary is a lossy, derived
// representation of a range of the transcript: useful because it is already
// condensed, never a replacement for the transcript.
//
// The same range must not reach the model twice. So when summary capture is
// on, PreCompact records how far the transcript goes instead of extracting
// from it, PostCompact's summary is extracted, and only if that succeeds is
// the transcript's read position moved up to the recorded point, with a
// note that a summary stands for that range. If the summary never arrives
// or its extraction fails, nothing moves and the next transcript extraction
// (at session end) reads the range itself. A session that never compacts is
// captured exactly as before.

// SummarySteering is what PreCompact prints when steering is on. Claude Code
// uses a PreCompact hook's stdout as instructions for the summary, so this
// changes the user's summaries and is a separate, explicit choice.
const SummarySteering = "In the summary, keep a short list titled \"Durable learnings\": root causes that were found, decisions and their reasons, and rules the user stated, each with the file paths involved. Leave out what only matters for the task at hand."

type coveredRange struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	By   string `json:"by"`   // the representation that stands for the range
	Hash string `json:"hash"` // of that representation
	N    int    `json:"n"`    // which compaction of the session
	At   string `json:"at"`
}

// compaction is what is known about a session's compactions.
type compaction struct {
	Session    string          `json:"session"`
	Transcript string          `json:"transcript"`
	Upto       int64           `json:"upto"` // transcript size when the last compaction began
	N          int             `json:"n"`    // compactions seen
	At         string          `json:"at"`
	Done       map[string]bool `json:"done,omitempty"` // summary hashes already extracted
}

func compactionPath(s *kb.Store, session string) string {
	return s.LocalPath("extract", "compactions", kb.Slugify(session)+".json")
}

func loadCompaction(s *kb.Store, session string) *compaction {
	c := &compaction{Session: session}
	if data, err := os.ReadFile(compactionPath(s, session)); err == nil {
		_ = json.Unmarshal(data, c)
	}
	if c.Done == nil {
		c.Done = map[string]bool{}
	}
	return c
}

func saveCompaction(s *kb.Store, c *compaction) error {
	data, _ := json.MarshalIndent(c, "", "  ")
	_, err := kb.WriteFileIfChanged(compactionPath(s, c.Session), data)
	return err
}

// updateCompaction changes a session's record under its lock, starting
// from what is on disk now. The hooks of one session can overlap (a summary
// still being extracted when the next compaction begins), and none of them
// may write back a copy it read earlier.
func updateCompaction(s *kb.Store, session string, change func(c *compaction)) error {
	unlock, err := s.LockLocal("compaction-" + kb.Slugify(session) + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	c := loadCompaction(s, session)
	change(c)
	return saveCompaction(s, c)
}

// NoteCompaction records, when a main session is about to compact, how far
// its transcript goes. Nothing is extracted here.
func NoteCompaction(s *kb.Store, session, transcript string) error {
	if session == "" || transcript == "" {
		return errors.New("no session or transcript")
	}
	st, err := os.Stat(transcript)
	if err != nil {
		return err
	}
	return updateCompaction(s, session, func(c *compaction) {
		c.Transcript, c.Upto, c.N, c.At = transcript, st.Size(), c.N+1, time.Now().UTC().Format(time.RFC3339)
	})
}

// QueuedSummary is a compaction summary ready to be extracted.
type QueuedSummary struct {
	File       string
	Session    string
	Transcript string
	Upto       int64
	N          int
}

// QueueSummary stores a compaction summary for extraction. skip says why it
// is not taken: there is then nothing to do, and the transcript keeps its
// read position so its own extraction covers the range.
func QueueSummary(s *kb.Store, session, summary string) (q QueuedSummary, skip string) {
	summary = strings.TrimSpace(summary)
	c := loadCompaction(s, session)
	switch {
	case session == "":
		return q, "no session id"
	case len(summary) < 200:
		return q, "the summary is too short to hold anything"
	case len([]rune(summary)) > MaxWholeChars-100:
		// It would have to be cut to fit one prompt, and a summary the
		// model saw part of must not stand for the whole range.
		return q, fmt.Sprintf("the summary is longer than one prompt takes (%d characters)", len([]rune(summary)))
	case c.Transcript == "" || c.N == 0:
		// Without the point the compaction began at, the range the summary
		// stands for is unknown, and taking it would send that range to
		// the model twice.
		return q, "no compaction was recorded for this session before the summary arrived"
	case c.Done[contentHash([]byte(summary))]:
		return q, "this summary was already extracted"
	}
	dir := s.LocalPath("extract", "summaries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return q, err.Error()
	}
	file := filepath.Join(dir, fmt.Sprintf("%s-%d.md", kb.Slugify(session), c.N))
	if err := os.WriteFile(file, []byte(summary+"\n"), 0o644); err != nil {
		return q, err.Error()
	}
	return QueuedSummary{File: file, Session: session, Transcript: c.Transcript, Upto: c.Upto, N: c.N}, ""
}

// SummaryOptions is one summary extraction.
type SummaryOptions struct {
	QueuedSummary
	Actor   kb.Actor
	Command []string
	DryRun  bool
	Out     interface{ Write([]byte) (int, error) }
}

// RunSummary extracts claims from a compaction summary and, if that
// succeeds, records that the summary stands for the transcript range up to
// the point the compaction began. The entries name the summary as their
// source and say that it is a summary: it is never recorded as the whole
// conversation having been read.
func RunSummary(s *kb.Store, o SummaryOptions) (*Result, error) {
	covers, err := filepath.Abs(o.Transcript)
	if err != nil {
		return nil, err
	}
	// Hold the transcript's lock for the whole run: its own extraction
	// waits, then starts after the range this summary took over.
	unlock, err := s.LockLocal(sourceLock(covers))
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := os.ReadFile(o.File)
	if err != nil {
		return nil, err
	}
	hash := contentHash([]byte(strings.TrimSpace(string(data))))
	c := loadCompaction(s, o.Session)
	st := loadState(s, covers)
	switch {
	case c.Done[hash]:
		return &Result{Skipped: "this summary was already extracted"}, nil
	case o.Upto <= st.Offset:
		// The transcript itself was read past this point already.
		return &Result{Skipped: "the range this summary stands for was already extracted from the transcript"}, nil
	}
	res, err := Run(s, Options{
		Path: o.File, Source: "claude", SessionID: o.Session, Actor: o.Actor, Command: o.Command, DryRun: o.DryRun, Out: outWriter(o.Out),
		Final: true, NoMinimum: true, Whole: true,
		Resource: fmt.Sprintf("claude session %s, compaction summary %d (a summary of part of the session, not the conversation itself)", o.Session, o.N),
	})
	if err != nil || o.DryRun {
		return res, err // nothing moves: the transcript's own extraction covers the range
	}
	st.Covered = append(st.Covered, coveredRange{From: st.Offset, To: o.Upto, By: "compaction-summary", Hash: hash, N: o.N, At: time.Now().UTC().Format(time.RFC3339)})
	st.Path, st.Offset, st.Updated = covers, o.Upto, time.Now().UTC().Format(time.RFC3339)
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(statePath(s, covers), raw, 0o644); err != nil {
		return res, err
	}
	// Into the record as it is now: another compaction may have begun
	// while the model was working, and its number and boundary must stay.
	return res, updateCompaction(s, o.Session, func(c *compaction) { c.Done[hash] = true })
}

func outWriter(w interface{ Write([]byte) (int, error) }) interface {
	Write([]byte) (int, error)
} {
	if w == nil {
		return os.Stderr
	}
	return w
}
