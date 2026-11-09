// Package cli implements the kb command line.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuehaipeng/agent-kb/internal/extract"
	"github.com/xuehaipeng/agent-kb/internal/install"
	"github.com/xuehaipeng/agent-kb/internal/kb"
	"github.com/xuehaipeng/agent-kb/internal/mcp"
)

// Version is set at build time.
var Version = "dev"

// App carries I/O and global options for one invocation.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	root   string
	as     string
}

const usage = `kb - shared, cross-checked knowledge for coding agents (agent-kb)

Retrieve (in a hub: current repo's project + global by default):
  kb search [query] [-f path]... [-p project] [--all] [--type t] [--trusted] [--deprecated] [--no-docs] [-n N] [--json]
                 (also lists matching sections of the repo's own docs: they are the source of truth)
  kb show <id>... [--raw]
  kb status [--brief] [-p project] [--all] [--json]

Rules (always-on conventions, delivered at session start):
  kb rules [-p project] [--all]      the rules that apply here, plus proposed ones awaiting confirmation
  kb add rule "<instruction>" -d <why> -e "user said so" [--global]
  kb rules import [--user|--repo] [files...]   share rules written for one agent (CLAUDE.md, .cursor/rules, ~/.codex/AGENTS.md, ...)

Record and evolve:
  kb add <fact|learning|decision|convention> <title> -d <one line> [-b body|-b -] [-r glob]... [-t tag]...
         [-e evidence] [--global [--for p1,p2]] [-p project [--new-project]] [--supersedes id] [--force]
  kb add --from <file|-> [type] [-p project] [-e evidence]   seed many entries (JSON array or JSON lines)
  kb update <id> -m <why> [-t title] [-d line] [-b body|-b -] [-r glob]... [--tags t,..] [-e evidence]
  kb verify <id> --confirm|--dispute -e <evidence>
  kb deprecate <id> -m <reason>
  kb resolve <id> --keep|--deprecate [-m note] --as human:<name>

Projects (hub mode):
  kb hub init [--remote <git url>]   create the multi-project hub (~/.kb or $KB_HOME)
  kb hub clone <git url>             use an existing hub on this machine (e.g. the Hermes host)
  kb hub import <repo>/knowledge [-p project]   move a repo bundle into the hub
  kb project [list | link <id>]      show/list projects; map this repo to a project

Cross-review:
  kb review submit <title> [-m summary] [--base ref] [--head ref] [-a assumption]...
  kb review list [--for-me|--mine] [--done] [-p project] [--all]
  kb review show <id>
  kb review verdict <id> --approve|--changes [-f finding]... [-m summary]
  kb review revise <id> [--head ref] [-m what changed]
  kb review close <id> [-m note]

Set up and maintain:
  kb init [--dir knowledge] [--install claude,codex,git-hooks]   single-repo bundle (repo mode)
  kb install <claude|codex|cursor|antigravity|hermes|git-hooks|all>... [--global|--repo]
  kb index [--quiet]          regenerate index.md files (deterministic)
  kb lint [--staged] [--fix]  deterministic checks; exit 1 on errors
  kb maintain                 index + pointer refresh + lint (cron-safe)
  kb sync [--no-push]         git pull --rebase, reindex, commit knowledge, push (hub or repo bundle)
  kb pointer                  print the always-on pointer text

Capture and integration:
  kb extract --transcript <file> [--source claude|codex|text] [--final] [--dry-run]
  kb extract --scan codex|claude [--days N]   (hub: routes each session to its project)
  kb extract --scan memories|claude-memory|codex-memory|hermes-memory|antigravity-memory [-p project]... [--force]
                              import what agents saved in their own memory (only changed files)
  kb hook claude              Claude Code hook entry point (reads hook JSON on stdin)
  kb mcp [--as actor]         stdio MCP server (kb_search, kb_show, kb_add, kb_verify, kb_review)

Global: --root <bundle or hub dir> (or KB_ROOT), --as <actor> (or KB_ACTOR, e.g. codex/gpt-5.2, human:alice)
Hub location: $KB_HOME (default ~/.kb). A repo with knowledge/kb.yaml uses its own bundle instead.
`

// Run executes the command line and returns an exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &App{Stdin: stdin, Stdout: stdout, Stderr: stderr}
	// Global flags may precede the command.
	for len(args) > 0 {
		switch {
		case args[0] == "--root" && len(args) > 1:
			a.root, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--root="):
			a.root, args = strings.TrimPrefix(args[0], "--root="), args[1:]
		case args[0] == "--as" && len(args) > 1:
			a.as, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--as="):
			a.as, args = strings.TrimPrefix(args[0], "--as="), args[1:]
		default:
			goto done
		}
	}
done:
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "kb", Version)
		return 0
	case "init":
		err = a.cmdInit(rest)
	case "install":
		err = a.cmdInstall(rest)
	case "search", "s":
		err = a.cmdSearch(rest)
	case "show":
		err = a.cmdShow(rest)
	case "add", "propose":
		err = a.cmdAdd(rest)
	case "verify":
		err = a.cmdVerify(rest)
	case "resolve":
		err = a.cmdResolve(rest)
	case "deprecate":
		err = a.cmdDeprecate(rest)
	case "update", "edit":
		err = a.cmdUpdate(rest)
	case "rules", "rule":
		err = a.cmdRules(rest)
	case "project", "projects":
		err = a.cmdProject(rest)
	case "hub":
		err = a.cmdHub(rest)
	case "review":
		err = a.cmdReview(rest)
	case "status":
		err = a.cmdStatus(rest)
	case "index":
		err = a.cmdIndex(rest)
	case "lint":
		return a.cmdLint(rest)
	case "maintain":
		return a.cmdMaintain(rest)
	case "sync":
		err = a.cmdSync(rest)
	case "pointer":
		err = a.cmdPointer(rest)
	case "extract":
		err = a.cmdExtract(rest)
	case "hook":
		return a.cmdHook(rest)
	case "mcp":
		err = a.cmdMCP(rest)
	default:
		err = fmt.Errorf("unknown command %q (see 'kb help')", cmd)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		fmt.Fprintln(stderr, "kb:", err)
		return 1
	}
	return 0
}

// globals registers --root/--as on a subcommand's flags too.
func (a *App) globals(f *flags) {
	f.str(&a.root, a.root, "bundle directory", "root")
	f.str(&a.as, a.as, "actor", "as")
}

func (a *App) store() (*kb.Store, error) { return kb.Discover(a.root) }

func (a *App) actor() (kb.Actor, error) { return kb.DetectActor(a.as) }

func (a *App) out(format string, args ...any) { fmt.Fprintf(a.Stdout, format, args...) }

func (a *App) json(v any) error {
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *App) cmdInit(args []string) error {
	f := newFlags("init")
	a.globals(f)
	dir := kb.DefaultBundleDir
	var targets []string
	f.str(&dir, dir, "bundle directory name", "dir")
	f.list(&targets, true, "install targets", "install")
	if _, err := f.parse(args); err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	root := wd
	if top, err := kb.Git(wd, "rev-parse", "--show-toplevel"); err == nil {
		root = top
	}
	s, res, err := install.Init(root, dir)
	if err != nil {
		return err
	}
	a.printResult(res)
	a.out("knowledge bundle ready at %s\n", s.Dir)
	for _, t := range expandTargets(targets) {
		r, err := install.Run(s, t, map[string]string{"scope": "repo"})
		if err != nil {
			return fmt.Errorf("install %s: %w", t, err)
		}
		a.out("\n[%s]\n", t)
		a.printResult(r)
	}
	if len(targets) == 0 {
		a.out("next: kb install claude codex git-hooks   (then commit %s/ and the pointer files)\n", dir)
		a.out("tip: working across several repos? 'kb hub init' keeps every project's knowledge in one place instead\n")
	}
	return nil
}

func expandTargets(ts []string) []string {
	var out []string
	for _, t := range ts {
		if t == "all" {
			out = append(out, "claude", "codex", "git-hooks")
			continue
		}
		out = append(out, t)
	}
	return out
}

func (a *App) printResult(r *install.Result) {
	for _, c := range r.Changed {
		a.out("wrote %s\n", c)
	}
	for _, n := range r.Notes {
		a.out("note: %s\n", n)
	}
}

func (a *App) cmdInstall(args []string) error {
	f := newFlags("install")
	a.globals(f)
	opts := map[string]string{}
	var skills string
	var global, repo bool
	f.str(&skills, "", "Hermes skills directory", "skills-dir")
	f.boolean(&global, "install user-wide (default with a hub)", "global")
	f.boolean(&repo, "install into the current repository (default with a repo bundle)", "repo")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("install what? one or more of: %s, all", strings.Join(install.Targets, ", "))
	}
	if global && repo {
		return errors.New("--global and --repo are exclusive")
	}
	opts["skills-dir"] = skills
	if global {
		opts["scope"] = "global"
	}
	if repo {
		opts["scope"] = "repo"
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	for _, t := range expandTargets(pos) {
		r, err := install.Run(s, t, opts)
		if err != nil {
			return fmt.Errorf("install %s: %w", t, err)
		}
		a.out("[%s]\n", t)
		a.printResult(r)
		if len(r.Changed) == 0 && len(r.Notes) == 0 {
			a.out("already up to date\n")
		}
	}
	return nil
}

type hitJSON struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"` // entry | doc
	Project     string   `json:"project,omitempty"`
	Path        string   `json:"path,omitempty"` // doc hits: repo-relative file
	Line        int      `json:"line,omitempty"` // doc hits: heading line
	Type        string   `json:"type,omitempty"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Trust       string   `json:"trust,omitempty"`
	Stale       bool     `json:"stale,omitempty"`
	StaleReason string   `json:"stale_reason,omitempty"`
	Anchored    string   `json:"anchored,omitempty"`
	Resources   []string `json:"resources,omitempty"`
	Score       float64  `json:"score"`
}

func (a *App) cmdSearch(args []string) error {
	f := newFlags("search")
	a.globals(f)
	var q kb.SearchQuery
	var asJSON bool
	f.list(&q.Files, true, "file you will touch", "f", "file")
	f.list(&q.Types, true, "type filter", "type")
	f.list(&q.Projects, true, "project(s) to search (hub)", "p", "project")
	f.boolean(&q.AllProjects, "search every project (hub)", "all")
	f.boolean(&q.TrustedOnly, "only verified/cross-checked/reviewed", "trusted")
	f.boolean(&q.IncludeDeprecated, "include deprecated", "deprecated")
	f.boolean(&q.NoDocs, "skip matching sections of the repo's own docs", "no-docs")
	f.integer(&q.Limit, 0, "max results", "n", "limit")
	f.boolean(&asJSON, "JSON output", "json")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	q.Text = strings.Join(pos, " ")
	if q.Text == "" && len(q.Files) == 0 {
		return errors.New("usage: kb search <query> [-f path]...")
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	hits, err := s.Search(q)
	if err != nil {
		return err
	}
	if asJSON {
		out := []hitJSON{}
		for _, h := range hits {
			m := h.Node.Meta()
			out = append(out, hitJSON{ID: h.Node.ID, Kind: "entry", Project: h.Node.Project, Type: h.Node.TypeKey, Title: m.Title, Description: m.Description, Trust: h.Trust.State, Stale: h.Trust.Stale, StaleReason: h.Trust.StaleReason, Anchored: h.PathMatch, Resources: m.Resources, Score: h.Score})
		}
		if q.Text != "" && !q.NoDocs {
			for _, d := range s.SearchDocs(q.Text, q.Projects, q.AllProjects, q.DocLimit) {
				out = append(out, hitJSON{ID: d.Section.Ref(), Kind: "doc", Project: d.Section.Project, Title: d.Section.Heading, Description: d.Section.Snippet(160), Path: d.Section.Path, Line: d.Section.Line, Score: d.Score})
			}
		}
		return a.json(out)
	}
	a.out("%s", s.RenderHits(hits, q))
	return nil
}

func (a *App) cmdShow(args []string) error {
	f := newFlags("show")
	a.globals(f)
	var raw bool
	f.boolean(&raw, "print the file as stored", "raw")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: kb show <id>...")
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	for i, id := range pos {
		n, err := s.Get(id)
		if err != nil {
			return err
		}
		if i > 0 {
			a.out("\n---\n\n")
		}
		if raw {
			data, err := os.ReadFile(n.Path)
			if err != nil {
				return err
			}
			a.out("%s", data)
			continue
		}
		a.out("%s", s.RenderNode(n))
	}
	return nil
}

func (a *App) readBody(v string) (string, error) {
	if v == "-" {
		data, err := io.ReadAll(a.Stdin)
		return string(data), err
	}
	return v, nil
}

func (a *App) cmdAdd(args []string) error {
	f := newFlags("add")
	a.globals(f)
	var in kb.AddInput
	var bodyFile string
	f.str(&in.Description, "", "one-line description", "d", "description")
	f.str(&in.Body, "", "body (- for stdin)", "b", "body")
	f.str(&bodyFile, "", "read body from file", "body-file")
	f.list(&in.Resources, true, "code glob", "r", "resource", "resources")
	f.list(&in.Tags, true, "tag", "t", "tag", "tags")
	f.list(&in.Supersedes, true, "id this replaces", "s", "supersedes")
	f.str(&in.Evidence, "", "how you know (makes the entry verified)", "e", "evidence")
	f.str(&in.Project, "", "target project (hub)", "p", "project")
	f.list(&in.Projects, true, "with --global: projects it applies to", "for")
	var global bool
	var from string
	f.boolean(&global, "store in the global scope shared by all projects (hub)", "global")
	f.boolean(&in.Force, "skip duplicate check", "force")
	f.boolean(&in.NewProject, "allow creating the --project if it does not exist yet", "new-project")
	f.str(&from, "", "seed many entries from a JSON array or JSON lines file (- for stdin)", "from")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if global {
		in.Project = kb.GlobalProject
	}
	if from != "" {
		if len(pos) > 0 {
			in.Type = pos[0] // default type for items that omit it
		}
		if in.Actor, err = a.actor(); err != nil {
			return err
		}
		return a.seed(from, in)
	}
	if len(pos) < 2 {
		return errors.New("usage: kb add <fact|learning|decision|convention> <title> -d <one line> [-b body] [-r glob]... [-e evidence] [--global]")
	}
	in.Type, in.Title = pos[0], strings.Join(pos[1:], " ")
	if bodyFile != "" {
		data, err := os.ReadFile(bodyFile)
		if err != nil {
			return err
		}
		in.Body = string(data)
	} else if in.Body, err = a.readBody(in.Body); err != nil {
		return err
	}
	if in.Actor, err = a.actor(); err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	res, err := s.Add(in)
	if err != nil {
		return err
	}
	a.out("%s", mcp.AddSummary(res))
	return nil
}

func (a *App) cmdVerify(args []string) error {
	f := newFlags("verify")
	a.globals(f)
	var confirm, dispute bool
	var evidence string
	f.boolean(&confirm, "the claim holds", "confirm", "c")
	f.boolean(&dispute, "the claim is wrong", "dispute")
	f.str(&evidence, "", "what you checked", "e", "evidence", "m")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || confirm == dispute {
		return errors.New("usage: kb verify <id> --confirm|--dispute -e <evidence>")
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	result := kb.ResultConfirmed
	if dispute {
		result = kb.ResultDisputed
	}
	n, t, err := s.Verify(kb.VerifyInput{Ref: pos[0], Result: result, Evidence: evidence, Actor: actor})
	if err != nil {
		return err
	}
	a.out("%s is now %s\n", n.ID, t.Badge())
	if t.State == kb.StateDisputed {
		a.out("a human decides: kb resolve %s --keep|--deprecate --as human:<name>\n", n.ID)
	}
	return nil
}

func (a *App) cmdResolve(args []string) error {
	f := newFlags("resolve")
	a.globals(f)
	var keep, deprecate bool
	var note string
	f.boolean(&keep, "keep (confirm) the claim", "keep")
	f.boolean(&deprecate, "deprecate the claim", "deprecate")
	f.str(&note, "", "note", "m", "note")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || keep == deprecate {
		return errors.New("usage: kb resolve <id> --keep|--deprecate [-m note] --as human:<name>")
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	n, t, err := s.Resolve(pos[0], keep, note, actor)
	if err != nil {
		return err
	}
	a.out("%s is now %s\n", n.ID, t.Badge())
	return nil
}

func (a *App) cmdDeprecate(args []string) error {
	f := newFlags("deprecate")
	a.globals(f)
	var reason string
	f.str(&reason, "", "reason", "m", "reason")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: kb deprecate <id> -m <reason>")
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	n, err := s.Deprecate(pos[0], reason, actor)
	if err != nil {
		return err
	}
	a.out("%s deprecated\n", n.ID)
	return nil
}

func (a *App) cmdStatus(args []string) error {
	f := newFlags("status")
	a.globals(f)
	var brief, asJSON bool
	var o kb.StatusOptions
	f.boolean(&brief, "only what needs attention", "brief")
	f.boolean(&asJSON, "JSON output", "json")
	f.list(&o.Projects, true, "project(s) (hub)", "p", "project")
	f.boolean(&o.AllProjects, "every project (hub)", "all")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	o.Me, _ = a.actor()
	r, err := s.StatusFor(o)
	if err != nil {
		return err
	}
	switch {
	case asJSON:
		return a.json(r)
	case brief:
		if b := r.Brief(); b != "" {
			a.out("%s\n", b)
		}
	default:
		a.out("%s", kb.RenderStatus(r))
	}
	return nil
}

func (a *App) cmdIndex(args []string) error {
	f := newFlags("index")
	a.globals(f)
	var quiet bool
	f.boolean(&quiet, "no output", "quiet", "q")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	changed, err := s.WriteIndex()
	if err != nil {
		return err
	}
	if !quiet {
		if len(changed) == 0 {
			a.out("indexes up to date\n")
		}
		for _, c := range changed {
			a.out("wrote %s\n", filepath.Join(s.Dir, c))
		}
	}
	return nil
}

func (a *App) cmdLint(args []string) int {
	f := newFlags("lint")
	a.globals(f)
	var staged, fix, quiet, asJSON bool
	f.boolean(&staged, "check immutability of staged changes", "staged")
	f.boolean(&fix, "regenerate indexes first", "fix")
	f.boolean(&quiet, "only print errors", "quiet", "q")
	f.boolean(&asJSON, "JSON output", "json")
	if _, err := f.parse(args); err != nil {
		fmt.Fprintln(a.Stderr, "kb:", err)
		return 2
	}
	s, err := a.store()
	if err != nil {
		fmt.Fprintln(a.Stderr, "kb:", err)
		return 2
	}
	if fix {
		if _, err := s.WriteIndex(); err != nil {
			fmt.Fprintln(a.Stderr, "kb:", err)
			return 2
		}
		if _, err := install.RefreshPointer(s); err != nil {
			fmt.Fprintln(a.Stderr, "kb:", err)
			return 2
		}
		s.Refresh()
	}
	issues, err := s.Lint(kb.LintOptions{Staged: staged})
	if err != nil {
		fmt.Fprintln(a.Stderr, "kb:", err)
		return 2
	}
	return a.reportIssues(issues, quiet, asJSON)
}

func (a *App) reportIssues(issues []kb.Issue, quiet, asJSON bool) int {
	errs := 0
	for _, i := range issues {
		if i.Severity == "error" {
			errs++
		}
	}
	if asJSON {
		if issues == nil {
			issues = []kb.Issue{}
		}
		_ = a.json(issues)
	} else {
		for _, i := range issues {
			if quiet && i.Severity != "error" {
				continue
			}
			a.out("%s\n", i)
		}
		if !quiet {
			a.out("%d error(s), %d warning(s)\n", errs, len(issues)-errs)
		}
	}
	if errs > 0 {
		return 1
	}
	return 0
}

func (a *App) cmdMaintain(args []string) int {
	f := newFlags("maintain")
	a.globals(f)
	if _, err := f.parse(args); err != nil {
		fmt.Fprintln(a.Stderr, "kb:", err)
		return 2
	}
	s, err := a.store()
	if err != nil {
		fmt.Fprintln(a.Stderr, "kb:", err)
		return 2
	}
	changed, err := s.WriteIndex()
	if err != nil {
		fmt.Fprintln(a.Stderr, "kb:", err)
		return 2
	}
	for _, c := range changed {
		a.out("wrote %s\n", c)
	}
	if r, err := install.RefreshPointer(s); err == nil {
		a.printResult(r)
	}
	s.Refresh()
	issues, err := s.Lint(kb.LintOptions{})
	if err != nil {
		fmt.Fprintln(a.Stderr, "kb:", err)
		return 2
	}
	r, err := s.Status(kb.Actor{})
	if err == nil {
		a.out("%s", kb.RenderStatus(r))
	}
	return a.reportIssues(issues, false, false)
}

func (a *App) cmdPointer(args []string) error {
	f := newFlags("pointer")
	a.globals(f)
	if _, err := f.parse(args); err != nil {
		return err
	}
	a.out("%s", install.Pointer())
	return nil
}

func (a *App) cmdMCP(args []string) error {
	f := newFlags("mcp")
	a.globals(f)
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	srv := &mcp.Server{Store: s}
	if a.as != "" {
		srv.Explicit = kb.ParseActor(a.as)
	}
	mcp.Version = Version
	return srv.Serve(a.Stdin, a.Stdout)
}

func (a *App) cmdExtract(args []string) error {
	f := newFlags("extract")
	a.globals(f)
	var o extract.Options
	var scan, cmdStr string
	var days int
	f.str(&o.Path, "", "transcript file", "transcript")
	f.str(&o.Source, "", "claude|codex|text", "source")
	f.str(&o.SessionID, "", "session id", "session")
	f.str(&scan, "", "scan an agent's session store: codex|claude", "scan")
	f.integer(&days, 3, "scan window in days", "days")
	f.str(&cmdStr, "", "override extractor command (space separated)", "cmd")
	f.boolean(&o.Final, "advance the offset even if too short", "final")
	f.boolean(&o.DryRun, "print the prompt, do not call a model", "dry-run")
	var thenSync, forceImport bool
	f.boolean(&thenSync, "run 'kb sync' afterwards (hub mode)", "sync")
	f.boolean(&forceImport, "memory scans: re-import unchanged files", "force")
	var scanProjects []string
	f.list(&scanProjects, true, "memory scans: only these projects (hub)", "p", "project")
	if _, err := f.parse(args); err != nil {
		return err
	}
	if thenSync {
		defer func() {
			if err := a.cmdSync(nil); err != nil {
				fmt.Fprintln(a.Stderr, "kb: sync after extract:", err)
			}
		}()
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	var command []string
	if cmdStr != "" {
		command = strings.Fields(cmdStr)
	}
	if scan == "memories" || strings.HasSuffix(scan, "-memory") {
		which := strings.TrimSuffix(scan, "-memory")
		if scan == "memories" {
			which = "all"
		}
		report := extract.ImportAll(s, extract.MemorySources(s, which), extract.ImportOptions{Command: command, DryRun: o.DryRun, Force: forceImport, Projects: scanProjects, Out: a.Stderr})
		a.out("%s\n", strings.Join(report, "\n"))
		return nil
	}
	if scan != "" {
		report, err := extract.Scan(s, extract.ScanOptions{Source: scan, Days: days, Command: command, DryRun: o.DryRun, Out: a.Stderr})
		if err != nil {
			return err
		}
		a.out("%s\n", strings.Join(report, "\n"))
		return nil
	}
	if o.Path == "" {
		return errors.New("usage: kb extract --transcript <file> [--source claude|codex|text] | --scan codex|claude")
	}
	if o.Source == "" {
		o.Source = guessSource(o.Path)
	}
	if o.Actor, err = a.actor(); err != nil {
		if o.Source == "codex" {
			o.Actor = kb.Actor{Producer: "codex"}
		} else if o.Source == "claude" {
			o.Actor = kb.Actor{Producer: "claude-code"}
		} else {
			return err
		}
	}
	o.Command = command
	o.Out = a.Stderr
	res, err := extract.Run(s, o)
	if err != nil {
		return err
	}
	switch {
	case o.DryRun:
		a.out("%s", res.Prompt)
	case res.Skipped != "":
		a.out("skipped: %s\n", res.Skipped)
	default:
		a.out("added %d claim(s), %d duplicate(s), %d already in the repo's docs\n", len(res.Added), res.Dupes, res.Documented)
		for _, id := range res.Added {
			a.out("  %s [unverified]\n", id)
		}
	}
	return nil
}

func guessSource(p string) string {
	switch {
	case strings.Contains(p, ".codex") || strings.Contains(filepath.Base(p), "rollout-"):
		return "codex"
	case strings.HasSuffix(p, ".jsonl"):
		return "claude"
	}
	return "text"
}
