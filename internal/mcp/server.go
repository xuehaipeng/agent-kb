// Package mcp serves the knowledge bundle over the Model Context Protocol
// (JSON-RPC 2.0 on stdio). It is a thin wrapper over the same operations the
// CLI uses, for clients that prefer tools over shell commands. There is no
// network listener by design.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/xuehaipeng/agent-kb/internal/kb"
)

// Version is reported in serverInfo.
var Version = "dev"

const baseInstructions = "Shared knowledge across agents and projects. Follow the rules below. Call kb_search before working on unfamiliar code; unverified/disputed/stale entries are leads to check. Record durable lessons with kb_add (pass evidence when you have it; type convention for rules the user states); fix stale or wrong entries with kb_update."

// Server handles one stdio session.
type Server struct {
	Store    *kb.Store
	Explicit kb.Actor // from --as; wins over clientInfo
	client   kb.Actor
	mu       sync.Mutex
	out      *json.Encoder
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads newline-delimited JSON-RPC messages until EOF.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	s.out = json.NewEncoder(w)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.send(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		s.handle(req)
	}
	return sc.Err()
}

func (s *Server) send(r response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.out.Encode(r)
}

func (s *Server) handle(req request) {
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	reply := func(result any, err *rpcError) {
		if isNotification {
			return
		}
		s.send(response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: err})
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.client = kb.ActorFromClientName(p.ClientInfo.Name)
		version := p.ProtocolVersion
		if version < "2024-11-05" {
			version = "2025-06-18"
		}
		instructions := baseInstructions
		if rs, err := s.Store.Rules(nil, false); err == nil {
			if r := s.Store.RenderRules(rs, false); r != "" {
				instructions += "\n\n" + r
			}
		}
		reply(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "agent-kb", "version": Version},
			"instructions":    instructions,
		}, nil)
	case "notifications/initialized", "notifications/cancelled", "initialized":
	case "ping":
		reply(map[string]any{}, nil)
	case "tools/list":
		reply(map[string]any{"tools": tools()}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			reply(nil, &rpcError{-32602, "invalid params"})
			return
		}
		text, err := s.call(p.Name, p.Arguments)
		if err != nil {
			reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "error: " + err.Error()}}, "isError": true}, nil)
			return
		}
		reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil)
	case "resources/list":
		reply(map[string]any{"resources": []any{}}, nil)
	case "prompts/list":
		reply(map[string]any{"prompts": []any{}}, nil)
	default:
		reply(nil, &rpcError{-32601, "method not found: " + req.Method})
	}
}

func (s *Server) actor() (kb.Actor, error) {
	if !s.Explicit.Zero() {
		return s.Explicit, nil
	}
	if !s.client.Zero() {
		return s.client, nil
	}
	return kb.DetectActor("")
}

func schema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

var (
	strP  = map[string]any{"type": "string"}
	strsP = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	boolP = map[string]any{"type": "boolean"}
)

func tools() []any {
	return []any{
		map[string]any{
			"name":        "kb_search",
			"description": "Search shared knowledge (current project + global by default). Returns one-line headers with trust badges. Pass files you will touch to get entries anchored to them; project or all_projects to widen.",
			"inputSchema": schema(map[string]any{
				"query": strP, "files": strsP, "types": strsP, "project": strP, "all_projects": boolP,
				"trusted_only": boolP, "include_deprecated": boolP, "limit": map[string]any{"type": "integer"},
			}),
		},
		map[string]any{
			"name":        "kb_show",
			"description": "Read full knowledge entries or review packets by id (from kb_search).",
			"inputSchema": schema(map[string]any{"ids": strsP}, "ids"),
		},
		map[string]any{
			"name":        "kb_add",
			"description": "Record durable, non-obvious knowledge. type: fact|learning|decision|convention. evidence (how you know) makes it verified. resources: code globs it depends on. global: shared by all projects (projects: which ones it applies to).",
			"inputSchema": schema(map[string]any{
				"type": strP, "title": strP, "description": strP, "body": strP, "evidence": strP,
				"resources": strsP, "tags": strsP, "supersedes": strsP, "project": strP, "global": boolP, "projects": strsP, "new_project": boolP,
			}, "type", "title", "description"),
		},
		map[string]any{
			"name":        "kb_update",
			"description": "Fix a stale or wrong entry in place (new revision). reason is required; evidence verifies the new content.",
			"inputSchema": schema(map[string]any{
				"id": strP, "reason": strP, "evidence": strP, "title": strP, "description": strP, "body": strP, "resources": strsP,
			}, "id", "reason"),
		},
		map[string]any{
			"name":        "kb_verify",
			"description": "Record that you checked an entry against the code (any agent, including its author). result: confirmed|disputed. evidence: what you checked.",
			"inputSchema": schema(map[string]any{
				"id": strP, "result": map[string]any{"type": "string", "enum": []string{"confirmed", "disputed"}}, "evidence": strP,
			}, "id", "result", "evidence"),
		},
		map[string]any{
			"name":        "kb_review",
			"description": "Cross-review. action=list (waiting for you) | show | submit (title, summary, base, head, assumptions) | verdict (id, approve, findings, summary) | revise (id, head, summary).",
			"inputSchema": schema(map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"list", "show", "submit", "verdict", "revise"}},
				"id":     strP, "title": strP, "summary": strP, "base": strP, "head": strP,
				"assumptions": strsP, "approve": boolP, "findings": strsP,
			}, "action"),
		},
	}
}

func (s *Server) call(name string, raw json.RawMessage) (string, error) {
	s.Store.Refresh()
	switch name {
	case "kb_search":
		var a struct {
			Query             string   `json:"query"`
			Files             []string `json:"files"`
			Types             []string `json:"types"`
			Project           string   `json:"project"`
			AllProjects       bool     `json:"all_projects"`
			TrustedOnly       bool     `json:"trusted_only"`
			IncludeDeprecated bool     `json:"include_deprecated"`
			Limit             int      `json:"limit"`
		}
		if err := json.Unmarshal(orEmpty(raw), &a); err != nil {
			return "", err
		}
		q := kb.SearchQuery{Text: a.Query, Files: a.Files, Types: a.Types, AllProjects: a.AllProjects, TrustedOnly: a.TrustedOnly, IncludeDeprecated: a.IncludeDeprecated, Limit: a.Limit}
		if a.Project != "" {
			q.Projects = []string{a.Project}
		}
		if q.Text == "" && len(q.Files) == 0 {
			return "", fmt.Errorf("give a query and/or files")
		}
		hits, err := s.Store.Search(q)
		if err != nil {
			return "", err
		}
		return s.Store.RenderHits(hits, q), nil
	case "kb_show":
		var a struct {
			IDs []string `json:"ids"`
			ID  string   `json:"id"`
		}
		if err := json.Unmarshal(orEmpty(raw), &a); err != nil {
			return "", err
		}
		if a.ID != "" {
			a.IDs = append(a.IDs, a.ID)
		}
		var parts []string
		for _, id := range a.IDs {
			n, err := s.Store.Get(id)
			if err != nil {
				parts = append(parts, "error: "+err.Error())
				continue
			}
			parts = append(parts, s.Store.RenderNode(n))
		}
		return strings.Join(parts, "\n---\n"), nil
	case "kb_add":
		var a struct {
			Type, Title, Description, Body, Evidence, Project string
			Resources, Tags, Supersedes, Projects             []string
			Global                                            bool
			NewProject                                        bool `json:"new_project"`
		}
		if err := json.Unmarshal(orEmpty(raw), &a); err != nil {
			return "", err
		}
		actor, err := s.actor()
		if err != nil {
			return "", err
		}
		if a.Global {
			a.Project = kb.GlobalProject
		}
		res, err := s.Store.Add(kb.AddInput{Type: a.Type, Title: a.Title, Description: a.Description, Body: a.Body, Evidence: a.Evidence, Resources: a.Resources, Tags: a.Tags, Supersedes: a.Supersedes, Project: a.Project, Projects: a.Projects, NewProject: a.NewProject, Actor: actor})
		if err != nil {
			return "", err
		}
		return AddSummary(res), nil
	case "kb_update":
		var a struct {
			ID, Reason, Evidence string
			Title, Description   *string
			Body                 *string
			Resources            []string
		}
		if err := json.Unmarshal(orEmpty(raw), &a); err != nil {
			return "", err
		}
		actor, err := s.actor()
		if err != nil {
			return "", err
		}
		in := kb.UpdateInput{Ref: a.ID, Reason: a.Reason, Evidence: a.Evidence, Title: a.Title, Description: a.Description, Body: a.Body, Actor: actor}
		if len(a.Resources) > 0 {
			in.Resources, in.SetResources = a.Resources, true
		}
		n, t, err := s.Store.Update(in)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s updated to revision %d [%s]\n", n.ID, t.Revision, t.Badge()), nil
	case "kb_verify":
		var a struct{ ID, Result, Evidence string }
		if err := json.Unmarshal(orEmpty(raw), &a); err != nil {
			return "", err
		}
		actor, err := s.actor()
		if err != nil {
			return "", err
		}
		n, t, err := s.Store.Verify(kb.VerifyInput{Ref: a.ID, Result: a.Result, Evidence: a.Evidence, Actor: actor})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s is now %s\n", n.ID, t.Badge()), nil
	case "kb_review":
		return s.review(raw)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func (s *Server) review(raw json.RawMessage) (string, error) {
	var a struct {
		Action, ID, Title, Summary, Base, Head string
		Assumptions, Findings                  []string
		Approve                                bool
	}
	if err := json.Unmarshal(orEmpty(raw), &a); err != nil {
		return "", err
	}
	actor, actorErr := s.actor()
	switch a.Action {
	case "list":
		f := kb.ReviewFilter{}
		if actorErr == nil {
			f.ForMe = actor
		}
		items, err := s.Store.Reviews(f)
		if err != nil {
			return "", err
		}
		return kb.RenderReviews(items), nil
	case "show":
		n, err := s.Store.Get(a.ID)
		if err != nil {
			return "", err
		}
		return s.Store.RenderReview(n), nil
	}
	if actorErr != nil {
		return "", actorErr
	}
	switch a.Action {
	case "submit":
		n, err := s.Store.ReviewSubmit(kb.SubmitInput{Title: a.Title, Summary: a.Summary, Base: a.Base, Head: a.Head, Assumptions: a.Assumptions, Actor: actor})
		if err != nil {
			return "", err
		}
		r := n.Meta().Review
		return fmt.Sprintf("submitted %s (%s..%s, %d files); reviewer family must differ from %s\n", n.ID, r.Base, r.Head, len(r.Files), r.ReviewerMustDifferFrom), nil
	case "verdict":
		n, st, err := s.Store.ReviewVerdict(kb.VerdictInput{Ref: a.ID, Approve: a.Approve, Findings: a.Findings, Summary: a.Summary, Actor: actor})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s is now %s\n", n.ID, st), nil
	case "revise":
		n, err := s.Store.ReviewRevise(a.ID, a.Head, a.Summary, actor)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s now points at %s and is pending review again\n", n.ID, n.Meta().Review.Head), nil
	}
	return "", fmt.Errorf("unknown review action %q", a.Action)
}

// AddSummary renders the result of an add for agents.
func AddSummary(res *kb.AddResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "added %s [%s]\n", res.Node.ID, res.Trust.Badge())
	if !res.Trust.Trusted() {
		fmt.Fprintf(&b, "recorded as a lead: add -e \"<how you know>\" when you have evidence, or another agent can 'kb verify' it\n")
	}
	if res.Redacted > 0 {
		fmt.Fprintf(&b, "redacted %d likely secret(s)\n", res.Redacted)
	}
	for _, d := range res.DocOverlap {
		fmt.Fprintf(&b, "overlaps repo doc %s (coverage %.2f): keep only what the doc doesn't say, or 'kb deprecate' this entry\n", d.Section.Ref(), d.Coverage)
	}
	for _, r := range res.Related {
		fmt.Fprintf(&b, "related: %s (%q, similarity %.2f): if they overlap, merge with 'kb update %s'\n", r.ID, r.Title, r.Score, r.ID)
	}
	return b.String()
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}
