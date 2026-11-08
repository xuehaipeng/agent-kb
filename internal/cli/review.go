package cli

import (
	"errors"
	"strings"

	"github.com/xuehaipeng/agent-kb/internal/kb"
)

func (a *App) cmdReview(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kb review submit|list|show|verdict|revise|close ...")
	}
	sub, rest := args[0], args[1:]
	f := newFlags("review " + sub)
	a.globals(f)
	switch sub {
	case "submit":
		var in kb.SubmitInput
		f.str(&in.Summary, "", "summary of the change", "m", "summary")
		f.str(&in.Base, "", "base ref (default: merge-base with upstream/main)", "base")
		f.str(&in.Head, "", "head ref (default: HEAD)", "head")
		f.list(&in.Assumptions, false, "key assumption for the reviewer to check", "a", "assumption")
		f.list(&in.Concepts, true, "knowledge id to check against", "c", "concept")
		pos, err := f.parse(rest)
		if err != nil {
			return err
		}
		in.Title = strings.Join(pos, " ")
		if in.Title == "" {
			return errors.New("usage: kb review submit <one-line task summary> [-m details] [--base ref] [-a assumption]...")
		}
		if in.Summary, err = a.readBody(in.Summary); err != nil {
			return err
		}
		if in.Actor, err = a.actor(); err != nil {
			return err
		}
		s, err := a.store()
		if err != nil {
			return err
		}
		n, err := s.ReviewSubmit(in)
		if err != nil {
			return err
		}
		r := n.Meta().Review
		a.out("submitted %s (%s..%s, %d file(s)); a reviewer from a family other than %s should run:\n  kb review show %s\n", n.ID, r.Base, r.Head, len(r.Files), r.ReviewerMustDifferFrom, n.ID)
		return nil

	case "list", "ls":
		var forMe, mine, all bool
		f.boolean(&forMe, "reviews waiting for your verdict", "for-me")
		f.boolean(&mine, "reviews you (your family) authored", "mine")
		f.boolean(&all, "include approved and closed", "all")
		if _, err := f.parse(rest); err != nil {
			return err
		}
		s, err := a.store()
		if err != nil {
			return err
		}
		filter := kb.ReviewFilter{All: all}
		if forMe || mine {
			me, err := a.actor()
			if err != nil {
				return err
			}
			if forMe {
				filter.ForMe = me
			}
			if mine {
				filter.MineBy = me
			}
		}
		items, err := s.Reviews(filter)
		if err != nil {
			return err
		}
		a.out("%s", kb.RenderReviews(items))
		return nil

	case "show":
		pos, err := f.parse(rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: kb review show <id>")
		}
		s, err := a.store()
		if err != nil {
			return err
		}
		n, err := s.Get(pos[0])
		if err != nil {
			return err
		}
		a.out("%s", s.RenderReview(n))
		return nil

	case "verdict":
		var in kb.VerdictInput
		var changes bool
		f.boolean(&in.Approve, "approve", "approve")
		f.boolean(&changes, "request changes", "changes", "request-changes")
		f.list(&in.Findings, false, "finding", "f", "finding")
		f.str(&in.Summary, "", "summary", "m", "summary")
		pos, err := f.parse(rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 || in.Approve == changes {
			return errors.New("usage: kb review verdict <id> --approve|--changes [-f finding]... [-m summary]")
		}
		in.Ref = pos[0]
		if in.Actor, err = a.actor(); err != nil {
			return err
		}
		s, err := a.store()
		if err != nil {
			return err
		}
		n, st, err := s.ReviewVerdict(in)
		if err != nil {
			return err
		}
		a.out("%s is now %s\n", n.ID, st)
		return nil

	case "revise":
		var head, note string
		f.str(&head, "", "new head ref (default HEAD)", "head")
		f.str(&note, "", "what changed", "m", "summary")
		pos, err := f.parse(rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: kb review revise <id> [--head ref] [-m what changed]")
		}
		actor, err := a.actor()
		if err != nil {
			return err
		}
		s, err := a.store()
		if err != nil {
			return err
		}
		n, err := s.ReviewRevise(pos[0], head, note, actor)
		if err != nil {
			return err
		}
		a.out("%s now points at %s and is pending review again\n", n.ID, n.Meta().Review.Head)
		return nil

	case "close":
		var note string
		f.str(&note, "", "note", "m")
		pos, err := f.parse(rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: kb review close <id> [-m note]")
		}
		actor, err := a.actor()
		if err != nil {
			return err
		}
		s, err := a.store()
		if err != nil {
			return err
		}
		n, err := s.ReviewClose(pos[0], note, actor)
		if err != nil {
			return err
		}
		a.out("%s closed\n", n.ID)
		return nil
	}
	return errors.New("unknown review command " + sub + " (submit|list|show|verdict|revise|close)")
}
