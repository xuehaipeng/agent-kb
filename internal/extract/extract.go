// Package extract turns agent session transcripts into knowledge proposals by
// condensing the transcript and asking a cheap model (any CLI that reads a
// prompt on stdin) for durable claims. Claims land as `unverified`.
package extract

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xuehaipeng/agent-kb/internal/kb"
	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// GuardEnv is set for the extractor and its model subprocess so that the
// model CLI's own session hooks do not recursively trigger extraction.
const GuardEnv = "KB_EXTRACTING"

// Message is one condensed transcript turn.
type Message struct {
	Role string
	Text string
}

// Transcript is the parsed new portion of a session log.
type Transcript struct {
	Messages  []Message
	CWD       string
	SessionID string
	NewOffset int64
}

var reminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// ParseJSONL reads a Claude Code or Codex JSONL transcript from offset.
// Only complete lines are consumed. Tool calls/results are skipped.
func ParseJSONL(path string, offset int64) (*Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if offset > st.Size() {
		offset = 0 // file was rotated or rewritten
	}
	t := &Transcript{NewOffset: offset}
	// Read the head for session metadata even when resuming mid-file.
	readHead(f, t)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	pos := offset
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial last line is left for next time
		}
		pos += int64(len(line))
		var obj map[string]any
		if json.Unmarshal(bytes.TrimSpace(line), &obj) != nil {
			continue
		}
		meta(obj, t)
		if m, ok := message(obj); ok {
			t.Messages = append(t.Messages, m)
		}
	}
	t.NewOffset = pos
	return t, nil
}

// ReadMeta reads only session metadata (cwd, session id) from a transcript.
func ReadMeta(path string) (*Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t := &Transcript{}
	readHead(f, t)
	return t, nil
}

func readHead(f *os.File, t *Transcript) {
	head := bufio.NewReader(io.LimitReader(f, 256*1024))
	for i := 0; i < 30 && (t.CWD == "" || t.SessionID == ""); i++ {
		line, err := head.ReadBytes('\n')
		if len(line) > 0 {
			var obj map[string]any
			if json.Unmarshal(line, &obj) == nil {
				meta(obj, t)
			}
		}
		if err != nil {
			break
		}
	}
}

func meta(obj map[string]any, t *Transcript) {
	if t.CWD == "" {
		if c, ok := obj["cwd"].(string); ok {
			t.CWD = c
		} else if p, ok := obj["payload"].(map[string]any); ok {
			if c, ok := p["cwd"].(string); ok {
				t.CWD = c
			}
		}
	}
	if t.SessionID == "" {
		for _, k := range []string{"sessionId", "session_id"} {
			if v, ok := obj[k].(string); ok {
				t.SessionID = v
			}
		}
		if obj["type"] == "session_meta" {
			if p, ok := obj["payload"].(map[string]any); ok {
				if v, ok := p["id"].(string); ok {
					t.SessionID = v
				}
			}
		}
	}
}

func message(obj map[string]any) (Message, bool) {
	if obj["type"] == "event_msg" || obj["isSidechain"] == true || obj["isMeta"] == true {
		return Message{}, false
	}
	var msg map[string]any
	if m, ok := obj["message"].(map[string]any); ok {
		msg = m
	} else if p, ok := obj["payload"].(map[string]any); ok && p["type"] == "message" {
		msg = p
	} else if _, ok := obj["role"]; ok {
		msg = obj
	}
	if msg == nil {
		return Message{}, false
	}
	role, _ := msg["role"].(string)
	if role != "user" && role != "assistant" {
		return Message{}, false
	}
	var parts []string
	switch c := msg["content"].(type) {
	case string:
		parts = append(parts, c)
	case []any:
		for _, it := range c {
			im, ok := it.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := im["type"].(string)
			if typ != "" && typ != "text" && typ != "input_text" && typ != "output_text" {
				continue
			}
			if s, ok := im["text"].(string); ok {
				parts = append(parts, s)
			}
		}
	}
	text := strings.TrimSpace(reminderRe.ReplaceAllString(strings.Join(parts, "\n"), ""))
	if text == "" {
		return Message{}, false
	}
	for _, skip := range []string{"<environment_context>", "<user_instructions>", "# AGENTS.md instructions", "<command-name>", "<local-command-stdout>", "Caveat: The messages below"} {
		if strings.HasPrefix(text, skip) {
			return Message{}, false
		}
	}
	return Message{Role: role, Text: text}, true
}

// Condense renders messages as a compact transcript, keeping the most recent
// content within maxChars.
func Condense(msgs []Message, perMsg, maxChars int) string {
	var blocks []string
	for _, m := range msgs {
		text := m.Text
		if r := []rune(text); len(r) > perMsg {
			text = string(r[:perMsg/2]) + "\n[...]\n" + string(r[len(r)-perMsg/2:])
		}
		blocks = append(blocks, strings.ToUpper(m.Role)+": "+text)
	}
	total := 0
	start := len(blocks)
	for start > 0 && total+len(blocks[start-1]) <= maxChars {
		start--
		total += len(blocks[start]) + 2
	}
	return strings.Join(blocks[start:], "\n\n")
}

// Claim is one proposal returned by the model.
type Claim struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	Resources   []string `json:"resources"`
	Tags        []string `json:"tags"`
}

// ParseClaims extracts the JSON array of claims from model output.
func ParseClaims(out string) ([]Claim, error) {
	s := strings.TrimSpace(out)
	i := strings.Index(s, "[")
	j := strings.LastIndex(s, "]")
	if i < 0 || j < i {
		return nil, fmt.Errorf("no JSON array in extractor output: %q", truncate(s, 200))
	}
	var claims []Claim
	if err := json.Unmarshal([]byte(s[i:j+1]), &claims); err != nil {
		return nil, fmt.Errorf("extractor output: %w", err)
	}
	return claims, nil
}

// Prompt builds the extraction prompt.
func Prompt(transcript string, existing []string, maxClaims int, types []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You extract durable project knowledge from a coding-agent session transcript, for a knowledge base shared by several AI coding agents working on the same repository.

Return ONLY a JSON array (no prose, no code fence) of at most %d objects shaped like:
{"type": "%s", "title": "specific, <= 90 chars", "description": "one line, <= 160 chars", "body": "2-6 sentences: what, why, and the evidence (file paths, commands, error messages)", "resources": ["repo-relative globs of the code this depends on"], "tags": ["short", "keywords"]}

Include only knowledge that is:
- non-obvious from reading the code or git log: root causes, gotchas, invariants, environment quirks, decisions with their reasons, conventions the user stated;
- likely to stay true and to matter in future sessions;
- supported by the transcript, not speculation.
Exclude task progress, TODOs, abandoned attempts without a lesson, generic programming advice, and any secrets, credentials or personal data.
Do not repeat knowledge that already exists (titles below). Returning [] is normal and expected when nothing qualifies.
`, maxClaims, strings.Join(types, "|"))
	if len(existing) > 0 {
		b.WriteString("\nExisting knowledge titles:\n")
		for _, t := range existing {
			b.WriteString("- " + t + "\n")
		}
	}
	b.WriteString("\nTranscript (oldest first):\n\n")
	b.WriteString(transcript)
	b.WriteString("\n")
	return b.String()
}

// Options configures one extraction run.
type Options struct {
	Path      string   // transcript (.jsonl) or plain text file
	Source    string   // claude | codex | text: picks the extractor command
	SessionID string   // optional; detected from the transcript
	Actor     kb.Actor // whose session this was
	Command   []string // override extractor command
	Final     bool     // advance the offset even if too little content (session end)
	DryRun    bool     // print the prompt instead of running the model
	Out       io.Writer
}

// Result summarises an extraction run.
type Result struct {
	Skipped string
	Added   []string
	Dupes   int
	Prompt  string
}

type state struct {
	Path    string `json:"path"`
	Offset  int64  `json:"offset"`
	Updated string `json:"updated"`
}

func statePath(s *kb.Store, path string) string {
	h := sha1.Sum([]byte(path))
	return s.LocalPath("extract", hex.EncodeToString(h[:8])+".json")
}

func loadState(s *kb.Store, path string) state {
	var st state
	if data, err := os.ReadFile(statePath(s, path)); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func saveState(s *kb.Store, path string, off int64) error {
	p := statePath(s, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, _ := json.Marshal(state{Path: path, Offset: off, Updated: time.Now().UTC().Format(time.RFC3339)})
	return os.WriteFile(p, data, 0o644)
}

// Run extracts claims from the new part of a transcript and adds them.
func Run(s *kb.Store, o Options) (*Result, error) {
	if o.Actor.Zero() {
		return nil, kb.ErrNoActor
	}
	res := &Result{}
	abs, err := filepath.Abs(o.Path)
	if err != nil {
		return nil, err
	}
	st := loadState(s, abs)
	var tr *Transcript
	if strings.HasSuffix(abs, ".jsonl") {
		tr, err = ParseJSONL(abs, st.Offset)
		if err != nil {
			return nil, err
		}
	} else {
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) < st.Offset {
			st.Offset = 0
		}
		tr = &Transcript{Messages: []Message{{Role: "notes", Text: string(data[st.Offset:])}}, NewOffset: int64(len(data))}
	}
	if o.SessionID == "" {
		o.SessionID = tr.SessionID
	}
	condensed := Condense(tr.Messages, 2500, 60000)
	if len(condensed) < s.Cfg.Extract.MinChars {
		res.Skipped = fmt.Sprintf("only %d chars of new transcript (min %d)", len(condensed), s.Cfg.Extract.MinChars)
		if o.Final && !o.DryRun {
			return res, saveState(s, abs, tr.NewOffset)
		}
		return res, nil
	}
	nodes, err := s.Nodes()
	if err != nil {
		return nil, err
	}
	var titles []string
	for _, n := range nodes {
		if n.TypeKey != "review" && s.Trust(n).State != kb.StateDeprecated {
			titles = append(titles, n.Meta().Title)
		}
	}
	sort.Strings(titles)
	if len(titles) > 150 {
		titles = titles[len(titles)-150:]
	}
	var types []string
	for _, t := range s.Cfg.TypeNames() {
		if t != "review" {
			types = append(types, t)
		}
	}
	prompt := Prompt(condensed, titles, s.Cfg.Extract.MaxClaims, types)
	if o.DryRun {
		res.Prompt = prompt
		return res, nil
	}
	cmd := o.Command
	if len(cmd) == 0 {
		cmd = s.Cfg.Extract.Commands[o.Source]
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("no extractor command for source %q (set extract.commands.%s in kb.yaml)", o.Source, o.Source)
	}
	out, err := runModel(cmd, prompt, time.Duration(s.Cfg.Extract.TimeoutSec)*time.Second)
	if err != nil {
		return nil, err
	}
	claims, err := ParseClaims(out)
	if err != nil {
		return nil, err
	}
	if len(claims) > s.Cfg.Extract.MaxClaims {
		claims = claims[:s.Cfg.Extract.MaxClaims]
	}
	srcID := "session"
	if o.SessionID != "" {
		srcID = "sess-" + truncate(kb.Slugify(o.SessionID), 12)
	}
	for _, c := range claims {
		body := strings.TrimSpace(c.Body)
		if body == "" {
			body = c.Description
		}
		body += fmt.Sprintf(" [^%s]", srcID)
		r, err := s.Add(kb.AddInput{
			Type: c.Type, Title: c.Title, Description: c.Description, Body: body,
			Tags: c.Tags, Resources: c.Resources, Actor: o.Actor,
			Sources: []okf.Source{{
				ID: srcID, Resource: fmt.Sprintf("%s session %s", o.Source, o.SessionID),
				Author: o.Actor.String(), LastModified: time.Now().UTC().Format(time.RFC3339),
			}},
		})
		var dup *kb.DuplicateError
		switch {
		case errors.As(err, &dup):
			res.Dupes++
		case err != nil:
			if o.Out != nil {
				fmt.Fprintf(o.Out, "skip %q: %v\n", c.Title, err)
			}
		default:
			res.Added = append(res.Added, r.Node.ID)
		}
	}
	return res, saveState(s, abs, tr.NewOffset)
}

func runModel(argv []string, prompt string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 4 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	// Run outside the repository so the model CLI does not load project
	// instructions or hooks, which would waste tokens and could recurse.
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), GuardEnv+"=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("extractor %s: %v: %s", argv[0], err, truncate(strings.TrimSpace(errb.String()), 300))
	}
	return out.String(), nil
}

// ScanOptions configures a sweep over an agent's session store.
type ScanOptions struct {
	Source  string // codex | claude
	Days    int
	Home    string // override ~/.codex or ~/.claude
	Command []string
	DryRun  bool
	Out     io.Writer
}

// Scan finds recent sessions of an agent whose cwd lies inside this repo and
// extracts each (offsets make repeated scans incremental).
func Scan(s *kb.Store, o ScanOptions) ([]string, error) {
	home := o.Home
	var pattern func(string) bool
	var root string
	var actor kb.Actor
	switch o.Source {
	case "codex":
		if home == "" {
			home = os.Getenv("CODEX_HOME")
		}
		if home == "" {
			h, _ := os.UserHomeDir()
			home = filepath.Join(h, ".codex")
		}
		root = filepath.Join(home, "sessions")
		pattern = func(name string) bool {
			return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
		}
		actor = kb.Actor{Producer: "codex"}
	case "claude":
		if home == "" {
			h, _ := os.UserHomeDir()
			home = filepath.Join(h, ".claude")
		}
		root = filepath.Join(home, "projects")
		pattern = func(name string) bool { return strings.HasSuffix(name, ".jsonl") }
		actor = kb.Actor{Producer: "claude-code"}
	default:
		return nil, fmt.Errorf("unknown scan source %q (codex or claude)", o.Source)
	}
	if o.Days <= 0 {
		o.Days = 3
	}
	cutoff := time.Now().Add(-time.Duration(o.Days) * 24 * time.Hour)
	repo := s.GitTop()
	if repo == "" {
		repo = s.CodeRoot
	}
	var files []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && pattern(info.Name()) && info.ModTime().After(cutoff) {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	var report []string
	for _, f := range files {
		tr, err := ReadMeta(f)
		if err != nil || tr.CWD == "" || !within(repo, tr.CWD) {
			continue
		}
		r, err := Run(s, Options{Path: f, Source: o.Source, Actor: actor, Command: o.Command, DryRun: o.DryRun, Final: true, Out: o.Out})
		switch {
		case err != nil:
			report = append(report, fmt.Sprintf("%s: error: %v", filepath.Base(f), err))
		case r.Skipped != "":
			report = append(report, fmt.Sprintf("%s: skipped (%s)", filepath.Base(f), r.Skipped))
		case o.DryRun:
			report = append(report, fmt.Sprintf("%s: would extract (%d-char prompt)", filepath.Base(f), len(r.Prompt)))
		default:
			report = append(report, fmt.Sprintf("%s: added %d, duplicates %d %v", filepath.Base(f), len(r.Added), r.Dupes, r.Added))
		}
	}
	if len(report) == 0 {
		report = append(report, fmt.Sprintf("no %s sessions for %s in the last %d day(s) under %s", o.Source, repo, o.Days, root))
	}
	return report, nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && !strings.HasPrefix(rel, "..")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
