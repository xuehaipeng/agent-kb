package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

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
	switch in.HookEventName {
	case "SessionStart":
		me, err := kb.DetectActor(a.as)
		if err != nil {
			me = kb.Actor{Producer: "claude-code"}
		}
		r, err := s.Status(me)
		if err == nil {
			if b := r.Brief(); b != "" {
				fmt.Fprintln(a.Stdout, b)
			}
		}
	case "PreCompact", "SessionEnd", "Stop":
		if os.Getenv(extract.GuardEnv) != "" || in.TranscriptPath == "" {
			return 0
		}
		if err := a.spawnExtract(s, in); err != nil {
			logLine(s, "hook: "+err.Error())
		}
	}
	return 0
}

// spawnExtract runs 'kb extract' detached so the session is not blocked.
func (a *App) spawnExtract(s *kb.Store, in hookInput) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
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
	}
	if err := os.MkdirAll(s.LocalPath(), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(s.LocalPath("extract.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "--- %s %s session=%s\n", in.HookEventName, filepath.Base(in.TranscriptPath), in.SessionID)
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), extract.GuardEnv+"=1")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Dir = s.Dir
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
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
