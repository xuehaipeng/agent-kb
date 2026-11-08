package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuehaipeng/agent-kb/internal/kb"
)

// cmdSync keeps a clone (typically the Hermes host) in step with the remote:
// pull --rebase, regenerate indexes, commit only the bundle, push. Each claim
// is its own file and log.md/index.md merge by union, so conflicts are rare;
// indexes are always regenerated after the pull.
func (a *App) cmdSync(args []string) error {
	f := newFlags("sync")
	a.globals(f)
	var noPush, quiet bool
	var msg string
	f.boolean(&noPush, "do not push", "no-push")
	f.boolean(&quiet, "no output unless something fails", "quiet", "q")
	f.str(&msg, "", "commit message", "m", "message")
	if _, err := f.parse(args); err != nil {
		return err
	}
	if quiet {
		a = &App{Stdin: a.Stdin, Stdout: io.Discard, Stderr: a.Stderr, root: a.root, as: a.as}
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	top := s.KnowledgeTop()
	if top == "" {
		return fmt.Errorf("sync needs %s inside a git repository (hub: 'kb hub init' creates one)", s.Dir)
	}
	rel, err := filepath.Rel(top, s.Dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return errors.New("knowledge directory is outside its git work tree")
	}
	rel = filepath.ToSlash(rel)
	// Serialise with kb writes (and their hub autocommits).
	unlock, err := s.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	upstream, _ := kb.Git(top, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	pull := func() error {
		if upstream == "" {
			return nil
		}
		if _, err := kb.Git(top, "pull", "--rebase", "--autostash", "--quiet"); err != nil {
			_, _ = kb.Git(top, "rebase", "--abort")
			return fmt.Errorf("pull failed (rebase aborted, nothing lost): %w", err)
		}
		return nil
	}
	if err := pull(); err != nil {
		return err
	}
	s.Refresh()
	if _, err := s.WriteIndex(); err != nil {
		return err
	}
	if _, err := kb.Git(top, "add", "-A", "--", rel); err != nil {
		return err
	}
	if _, err := kb.Git(top, "diff", "--cached", "--quiet", "--", rel); err == nil {
		a.out("knowledge up to date")
		if upstream != "" {
			a.out(" with %s", upstream)
		}
		a.out("\n")
		if noPush {
			return nil
		}
		if upstream != "" {
			// Push commits made locally (autocommits, earlier --no-push runs).
			ahead, _ := kb.Git(top, "rev-list", "--count", "@{upstream}..HEAD")
			if ahead == "" || ahead == "0" {
				return nil
			}
		}
	} else {
		if msg == "" {
			who := a.as
			if who == "" {
				who = os.Getenv("KB_ACTOR")
			}
			if who == "" {
				who, _ = os.Hostname()
			}
			msg = "kb: sync knowledge from " + who
		}
		if _, err := kb.Git(top, "commit", "--quiet", "--no-verify", "-m", msg, "--", rel); err != nil {
			return err
		}
		a.out("committed %s/ changes\n", rel)
	}
	if noPush {
		return nil
	}
	if upstream == "" {
		if _, err := kb.Git(top, "remote", "get-url", "origin"); err != nil {
			a.out("no remote 'origin': knowledge is committed locally only (git -C %s remote add origin <url>)\n", top)
			return nil
		}
	}
	branch, err := kb.Git(top, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	pushArgs := []string{"push", "--quiet"}
	if upstream == "" {
		pushArgs = append(pushArgs, "-u", "origin", branch)
	}
	if _, err := kb.Git(top, pushArgs...); err != nil {
		// Someone pushed in between: rebase once more and retry.
		if perr := pull(); perr != nil {
			return perr
		}
		s.Refresh()
		if changed, _ := s.WriteIndex(); len(changed) > 0 {
			_, _ = kb.Git(top, "add", "-A", "--", rel)
			_, _ = kb.Git(top, "commit", "--quiet", "--no-verify", "-m", "kb: reindex after sync", "--", rel)
		}
		if _, err := kb.Git(top, pushArgs...); err != nil {
			return err
		}
	}
	a.out("pushed %s\n", branch)
	return nil
}
