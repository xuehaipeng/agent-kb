package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/xuehaipeng/agent-kb/internal/install"
	"github.com/xuehaipeng/agent-kb/internal/kb"
)

func (a *App) cmdUpdate(args []string) error {
	f := newFlags("update")
	a.globals(f)
	var in kb.UpdateInput
	var title, desc, body string
	var resources, tags []string
	f.str(&title, "", "new title", "t", "title")
	f.str(&desc, "", "new one-line description", "d", "description")
	f.str(&body, "", "new body (- for stdin)", "b", "body")
	f.list(&resources, true, "code globs (replaces the list)", "r", "resource", "resources")
	f.list(&tags, true, "tags (replaces the list)", "tags")
	f.str(&in.Reason, "", "why it changed", "m", "reason")
	f.str(&in.Evidence, "", "how you know the new content holds", "e", "evidence")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: kb update <id> -m <why> [-t title] [-d line] [-b body|-b -] [-r glob]... [--tags ...] [-e evidence]")
	}
	in.Ref = pos[0]
	set := func(name string) bool { return f.fs.Lookup(name) != nil && flagSet(f, name) }
	if set("t") || set("title") {
		in.Title = &title
	}
	if set("d") || set("description") {
		in.Description = &desc
	}
	if set("b") || set("body") {
		if body, err = a.readBody(body); err != nil {
			return err
		}
		in.Body = &body
	}
	if len(resources) > 0 {
		in.Resources, in.SetResources = resources, true
	}
	if len(tags) > 0 {
		in.Tags, in.SetTags = tags, true
	}
	if in.Actor, err = a.actor(); err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	n, t, err := s.Update(in)
	if err != nil {
		return err
	}
	a.out("%s updated to revision %d [%s]\n", n.ID, t.Revision, t.Ack())
	if !t.Trusted() {
		a.out("pass -e \"<evidence>\" next time to record it as verified, or confirm with 'kb verify %s --confirm -e ...'\n", n.ID)
	}
	return nil
}

func flagSet(f *flags, name string) bool {
	found := false
	f.fs.Visit(func(fl *flagT) {
		if fl.Name == name {
			found = true
		}
	})
	return found
}

func (a *App) cmdProject(args []string) error {
	f := newFlags("project")
	a.globals(f)
	var coRepo, coPath string
	f.str(&coRepo, "", "checkout set/clear: repository identity (e.g. github.com/org/repo)", "repo")
	f.str(&coPath, "", "checkout set: work tree to use as the baseline (default: the current one)", "path")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	if !s.Hub {
		a.out("repo mode: this repository uses its own bundle at %s (projects apply to hub mode; see 'kb hub init')\n", s.Dir)
		return nil
	}
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "", "show":
		if s.Project == "" {
			a.out("not inside a project repository (hub %s); 'kb project list' shows all projects\n", s.Dir)
			return nil
		}
		pf, exists := s.ProjectFiles()[s.Project]
		a.out("project: %s\nrepo: %s\ncheckout: %s\n", s.Project, s.RepoKey, s.CodeRoot)
		if !exists {
			a.out("(new: created on the first 'kb add' here; 'kb project link <id>' maps this repo to an existing project)\n")
		} else if len(pf.Repos) > 0 {
			a.out("repos in project: %s\n", strings.Join(pf.Repos, ", "))
		}
		return nil
	case "list", "ls":
		nodes, err := s.Nodes()
		if err != nil {
			return err
		}
		counts := map[string]int{}
		for _, n := range nodes {
			if n.TypeKey != "review" && s.Trust(n).State != kb.StateDeprecated {
				counts[n.Project]++
			}
		}
		pfs := s.ProjectFiles()
		var ids []string
		for id := range pfs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		a.out("%-24s %6s  %s\n", "PROJECT", "ACTIVE", "REPOS / LOCAL CHECKOUT")
		a.out("%-24s %6d  (shared by all projects)\n", kb.GlobalProject, counts[kb.GlobalProject])
		for _, id := range ids {
			mark := " "
			if id == s.Project {
				mark = "*"
			}
			info := strings.Join(pfs[id].Repos, ", ")
			if co := s.CheckoutFor(id); co != "" {
				info += "  " + co
			}
			a.out("%-24s %6d  %s\n", mark+id, counts[id], info)
		}
		return nil
	case "link":
		if len(pos) != 2 {
			return errors.New("usage: kb project link <project-id>   (run inside the repo to map)")
		}
		if err := s.LinkProject(pos[1]); err != nil {
			return err
		}
		a.out("%s now belongs to project %s\n", s.RepoKey, pos[1])
		return nil
	case "checkout", "checkouts":
		return a.projectCheckout(s, pos[1:], coRepo, coPath)
	}
	return fmt.Errorf("unknown project command %q (show|list|link|checkout)", sub)
}

// projectCheckout lists a project's checkouts on this machine and sets or
// clears which one is the baseline for code freshness. The choice is stored
// in the gitignored registry: it never leaves this machine.
func (a *App) projectCheckout(s *kb.Store, pos []string, repo, path string) error {
	const usage = "usage: kb project checkout list [project] | set [project] [--repo <repo-id>] [--path <checkout>] | clear [project] [--repo <repo-id>]"
	action := "list"
	if len(pos) > 0 {
		action, pos = pos[0], pos[1:]
	}
	project := s.Project
	if len(pos) > 0 {
		project = pos[0]
	}
	if project == "" || len(pos) > 1 {
		return errors.New(usage + "\n(outside a project repository, name the project)")
	}
	switch action {
	case "list", "ls":
		a.out("%s", s.RenderCheckouts(project))
		return nil
	case "set":
		if path == "" {
			if path = s.CodeRoot; path == "" {
				return errors.New("--path <checkout> is required outside a git work tree")
			}
		}
		id, top, err := s.SetPreferredCheckout(project, repo, path)
		if err != nil {
			return err
		}
		a.out("baseline for %s in project %s on this machine: %s\n", id, project, top)
		return nil
	case "clear":
		id, err := s.ClearPreferredCheckout(project, repo)
		if err != nil {
			return err
		}
		a.out("cleared the preferred checkout for %s in project %s\n", id, project)
		return nil
	}
	return errors.New(usage)
}

func (a *App) cmdHub(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kb hub init [--remote url] | kb hub clone <url> | kb hub import <bundle> [-p project]")
	}
	sub, rest := args[0], args[1:]
	f := newFlags("hub " + sub)
	a.globals(f)
	dir := kb.HubHome()
	f.str(&dir, dir, "hub directory", "dir")
	switch sub {
	case "init":
		var remote string
		f.str(&remote, "", "git remote for sharing (e.g. a private GitHub repo)", "remote")
		if _, err := f.parse(rest); err != nil {
			return err
		}
		res, err := install.HubInit(dir, remote)
		if err != nil {
			return err
		}
		a.printResult(res)
		a.out("hub ready at %s\nnext: kb install claude codex   (user-wide: every repo gets the pointer)\n", dir)
		if remote == "" {
			a.out("      add a private remote to share with other machines/Hermes: git -C %s remote add origin <url> && kb sync\n", dir)
		}
		if dir != kb.HubHome() {
			a.out("      export KB_HOME=%s\n", dir)
		}
		return nil
	case "clone":
		pos, err := f.parse(rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: kb hub clone <git url> [--dir path]")
		}
		if err := install.HubClone(pos[0], dir); err != nil {
			return err
		}
		a.out("hub cloned to %s\n", dir)
		return nil
	case "import":
		var project string
		f.str(&project, "", "target project id (default: derived from the bundle's repo)", "p", "project")
		pos, err := f.parse(rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: kb hub import <repo>/knowledge [-p project]")
		}
		if !kb.HubExists() && os.Getenv("KB_ROOT") == "" {
			return errors.New("no hub yet: run 'kb hub init' first")
		}
		s, err := kb.DiscoverAt(kb.HubHome(), pos[0])
		if err != nil {
			return err
		}
		n, id, err := install.HubImport(s, pos[0], project)
		if err != nil {
			return err
		}
		a.out("imported %d entr(ies) into project %s\n", n, id)
		return nil
	}
	return fmt.Errorf("unknown hub command %q (init|clone|import)", sub)
}
