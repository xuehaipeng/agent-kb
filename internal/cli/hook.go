package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/xuehaipeng/agent-kb/internal/extract"
	"github.com/xuehaipeng/agent-kb/internal/kb"
)

// hookInput is the JSON Claude Code passes to command hooks on stdin.
type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Source         string `json:"source"`
	Reason         string `json:"reason"`
	Trigger        string `json:"trigger"`
	// Set when the event belongs to a subagent rather than the main agent.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
	// Tool events.
	CompactSummary string `json:"compact_summary"` // PostCompact
	ToolName       string `json:"tool_name"`
	ToolInput      struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

// fileContextTimeout bounds the lookup the file-context hook does inside a
// tool call. Past it the hook says nothing: late context is worth less than
// a stalled tool.
const fileContextTimeout = 1500 * time.Millisecond

// hookContext prints additionalContext for events that take context only
// as JSON (SubagentStart, PreToolUse). It sets no decision.
func (a *App) hookContext(event, text string) {
	fmt.Fprint(a.Stdout, kb.HookContext(event, text))
}

// cmdHook never fails the agent: errors go to the local log, exit code is 0.
func (a *App) cmdHook(args []string) int {
	if len(args) == 0 || args[0] != "claude" {
		fmt.Fprintln(a.Stderr, "usage: kb hook claude   (reads Claude Code hook JSON on stdin)")
		return 0
	}
	data, _ := io.ReadAll(io.LimitReader(a.Stdin, 1<<20))
	var in hookInput
	_ = json.Unmarshal(data, &in)
	if in.CWD != "" && a.root == "" && os.Getenv("KB_ROOT") == "" {
		_ = os.Chdir(in.CWD)
	}
	s, err := a.store()
	if err != nil {
		return 0 // not a kb-enabled repository
	}
	if len(args) > 1 && args[1] == "file" {
		a.hookFileContext(s, in)
		return 0
	}
	switch in.HookEventName {
	case "SessionStart":
		me, err := kb.DetectActor(a.as)
		if err != nil {
			me = kb.Actor{Producer: "claude-code"}
		}
		// Rules are pushed into the session; everything else is pulled. Both
		// the rules and the brief follow the session's project: outside any
		// project that is the global scope only, never every project.
		// A subagent's own SessionStart (after it compacts): it gets the
		// complete rule set, like at its start. Nothing is assumed about
		// what it loads natively.
		if in.AgentID != "" {
			if d, err := s.AgentDelivery(kb.SurfaceSessionStart); err == nil {
				fmt.Fprint(a.Stdout, d.Text)
				s.RecordDelivery(in.SessionID, in.AgentID, in.AgentType, "SessionStart:"+in.Source, d, false, in.Source == "compact")
			}
			return 0
		}
		// The whole payload is fitted to one budget: Claude Code replaces a
		// hook's stdout with a short preview once it passes 10,000
		// characters, so an oversized rule list would mostly go missing.
		// It is printed on every start, resume, clear, compaction and fork:
		// a session seen before may still hold rules that have changed.
		if d, err := s.SessionStartDelivery(me); err == nil {
			fmt.Fprint(a.Stdout, d.Text)
			s.RecordDelivery(in.SessionID, "", "", "SessionStart:"+in.Source, d, d.Native > 0, in.Source == "compact")
		}
		// Hub: pull what other machines (e.g. Hermes) recorded, in the background.
		if s.Hub && hasOrigin(s) && os.Getenv(extract.GuardEnv) == "" {
			if err := a.spawn(s, in, []string{"--root", s.Dir, "sync", "--quiet"}, "sync"); err != nil {
				logLine(s, "hook: "+err.Error())
			}
		}
	case "SubagentStart":
		// A subagent starts with its own context: it does not see what
		// SessionStart gave the main agent, and may not load CLAUDE.md or
		// the rules files (Explore, Plan and agents that omit them do not).
		// So it gets the complete applicable rule set here, whatever the
		// main agent received.
		if d, err := s.AgentDelivery(kb.SurfaceSubagentStart); err == nil && d.Text != "" {
			a.hookContext("SubagentStart", d.Text)
			s.RecordDelivery(in.SessionID, orMain(in.AgentID), in.AgentType, "SubagentStart", d, false, false)
		}
	case "PreToolUse":
		// Opt-in rule refresh: when the rules changed since this agent last
		// received them, hand it the current set before it goes on. A
		// subagent keeps what SubagentStart gave it until then, and a
		// running main session only gets rules again at its next start.
		st := s.LoadDelivery(in.SessionID)
		last := st.Agents[orMain(in.AgentID)]
		if last != nil && last.Digest == s.RuleDigest() {
			return 0
		}
		d, err := s.AgentDelivery(kb.SurfaceRuleRefresh)
		if err != nil {
			return 0
		}
		switch {
		case d.Text != "":
			a.hookContext("PreToolUse", kb.RuleRefreshNotice+d.Text)
		case last != nil && last.Rules > 0:
			// The last rule it holds was retired or no longer applies here:
			// say so, or it goes on following an instruction that is gone.
			a.hookContext("PreToolUse", "kb: the rules you received from kb earlier no longer apply here, and no kb rules apply now. Stop following them unless the user says otherwise.")
		}
		// Recorded even when there is nothing to hand over, so the agent
		// is not looked at again on every tool call.
		s.RecordDelivery(in.SessionID, orMain(in.AgentID), in.AgentType, "PreToolUse:refresh", d, false, false)
	case "PostCompact":
		// Opt-in: the summary of a main session's compaction is extracted,
		// and stands for the part of the transcript before it. A subagent's
		// compaction is not captured: its summary belongs to another
		// conversation than the session's transcript.
		if capture, _ := s.SummaryCapture(); !capture {
			return 0
		}
		if in.AgentID != "" {
			logLine(s, "hook: compaction summary of subagent "+in.AgentID+" not captured (main sessions only)")
			return 0
		}
		q, skip := extract.QueueSummary(s, in.SessionID, in.CompactSummary)
		if skip != "" {
			logLine(s, "hook: compaction summary of "+in.SessionID+" not captured: "+skip+" (the transcript is read at session end instead)")
			return 0
		}
		if os.Getenv(extract.GuardEnv) != "" {
			return 0
		}
		if err := a.spawn(s, in, a.summaryArgs(s, q), "summary"); err != nil {
			logLine(s, "hook: "+err.Error())
		}
	case "PreCompact", "SessionEnd", "Stop":
		// With summary capture on, a main session's compaction is captured
		// from its summary, after the fact. Here only the point the
		// compaction begins at is recorded, so the same range is not also
		// extracted from the transcript. Steering is printed only when it
		// was chosen: PreCompact stdout becomes instructions for the summary.
		if capture, steering := s.SummaryCapture(); capture && in.HookEventName == "PreCompact" && in.AgentID == "" && in.TranscriptPath != "" {
			if err := extract.NoteCompaction(s, in.SessionID, in.TranscriptPath); err != nil {
				logLine(s, "hook: "+err.Error())
			} else {
				if steering {
					fmt.Fprintln(a.Stdout, extract.SummarySteering)
				}
				return 0
			}
		}
		if os.Getenv(extract.GuardEnv) != "" || in.TranscriptPath == "" {
			return 0
		}
		if err := a.spawnExtract(s, in); err != nil {
			logLine(s, "hook: "+err.Error())
		}
		// Share what Claude saved in its own memory (only changed files are read).
		if in.HookEventName == "SessionEnd" {
			if err := a.spawn(s, in, []string{"--root", s.Dir, "extract", "--scan", "claude-memory"}, "memory"); err != nil {
				logLine(s, "hook: "+err.Error())
			}
		}
	}
	return 0
}

// spawnExtract runs 'kb extract' detached so the session is not blocked.
func (a *App) spawnExtract(s *kb.Store, in hookInput) error {
	actor := a.as
	if actor == "" {
		actor = os.Getenv("KB_ACTOR")
	}
	if actor == "" {
		actor = "claude-code"
	}
	args := []string{"--root", s.Dir, "--as", actor, "extract", "--transcript", in.TranscriptPath, "--source", "claude", "--session", in.SessionID}
	if in.HookEventName == "SessionEnd" {
		args = append(args, "--final")
		if s.Hub && hasOrigin(s) {
			args = append(args, "--sync")
		}
	}
	return a.spawn(s, in, args, "extract")
}

// summaryArgs is the detached 'kb extract --summary' run for a queued summary.
func (a *App) summaryArgs(s *kb.Store, q extract.QueuedSummary) []string {
	actor := a.as
	if actor == "" {
		actor = os.Getenv("KB_ACTOR")
	}
	if actor == "" {
		actor = "claude-code"
	}
	return []string{"--root", s.Dir, "--as", actor, "extract", "--summary", q.File, "--session", q.Session, "--covers", q.Transcript,
		"--upto", fmt.Sprint(q.Upto), "--compaction", fmt.Sprint(q.N)}
}

// spawn starts a detached kb subprocess, logging to .kb/extract.log. It runs
// in the session's directory so a hub attaches the right project.
func (a *App) spawn(s *kb.Store, in hookInput, args []string, label string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.LocalPath(), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(s.LocalPath("extract.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "--- %s %s %s session=%s\n", in.HookEventName, label, filepath.Base(in.TranscriptPath), in.SessionID)
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), extract.GuardEnv+"=1")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Dir = s.Dir
	if s.CodeRoot != "" {
		cmd.Dir = s.CodeRoot
	} else if in.CWD != "" {
		cmd.Dir = in.CWD
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// hookFileContext is the opt-in file-context hook: after a successful Read,
// Edit or Write (and, in "pre" mode, before an Edit or Write) it hands the
// agent what is recorded about that file. It adds context only: it never
// decides whether the tool may run.
func (a *App) hookFileContext(s *kb.Store, in hookInput) {
	var pre bool
	mode := s.FileContextMode()
	switch {
	case mode == "":
		return // off: a hook entry left behind in some settings file changes nothing
	case in.HookEventName == "PostToolUse" && (in.ToolName == "Read" || in.ToolName == "Edit" || in.ToolName == "Write"):
	case in.HookEventName == "PreToolUse" && mode == kb.FileContextPre && (in.ToolName == "Edit" || in.ToolName == "Write"):
		pre = true
	default:
		return // not a file access this hook covers (a shell command, another tool)
	}
	// Only the lookup is timed, and it records nothing. What was given is
	// recorded after it has been printed: a lookup that is dropped for
	// taking too long leaves no trace, so the next touch of the file tries
	// again instead of treating those entries as already delivered.
	done := make(chan kb.FileContext, 1)
	go func() { done <- s.PlanFileContext(in.SessionID, in.AgentID, in.ToolInput.FilePath, pre) }()
	select {
	case fc := <-done:
		if fc.Text != "" {
			a.hookContext(in.HookEventName, fc.Text)
			s.RecordFileContext(in.SessionID, in.AgentID, in.AgentType, fc)
		}
	case <-fileContextDeadline():
		logLine(s, "hook: file context lookup took longer than "+fileContextTimeout.String()+" and was dropped")
	}
}

// fileContextDeadline is when a lookup is given up. KB_FILE_CONTEXT_TIMEOUT_MS
// overrides it (tests, slow disks).
func fileContextDeadline() <-chan time.Time {
	d := fileContextTimeout
	if v, err := strconv.Atoi(os.Getenv("KB_FILE_CONTEXT_TIMEOUT_MS")); err == nil && v >= 0 {
		d = time.Duration(v) * time.Millisecond
	}
	return time.After(d)
}

func orMain(agent string) string {
	if agent == "" {
		return "main"
	}
	return agent
}

func hasOrigin(s *kb.Store) bool {
	top := s.KnowledgeTop()
	if top == "" {
		return false
	}
	_, err := kb.Git(top, "remote", "get-url", "origin")
	return err == nil
}

func logLine(s *kb.Store, msg string) {
	_ = os.MkdirAll(s.LocalPath(), 0o755)
	f, err := os.OpenFile(s.LocalPath("extract.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, msg)
}
