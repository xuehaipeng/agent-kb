package okf

import (
	"strings"
	"testing"
)

const sample = `---
type: Learning
title: pnpm test hangs under Node 24
description: Vitest worker pool deadlocks.
tags: [testing, vitest]
custom_key: keep me   # unknown keys survive
generated: { by: claude-code/opus-4.5, at: 2026-11-08T14:12:00-08:00 }
verified:
  - { by: codex/gpt-5.2, at: 2026-11-08T16:40:00-08:00 }
status: stable
stale_after: 2027-02-08T00:00:00-08:00
sources:
  - id: sess-1
    resource: raw/sessions/x.md
---
Body text.[^sess-1]
`

func TestParseRoundTrip(t *testing.T) {
	d, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if d.Meta.Type != "Learning" || d.Meta.Generated.By != "claude-code/opus-4.5" {
		t.Fatalf("bad meta: %+v", d.Meta)
	}
	if d.Meta.Generated.At != "2026-11-08T14:12:00-08:00" {
		t.Fatalf("timestamp not preserved as text: %q", d.Meta.Generated.At)
	}
	if len(d.Meta.Verified) != 1 || len(d.Meta.Sources) != 1 || d.Meta.Sources[0].ID != "sess-1" {
		t.Fatalf("bad lists: %+v", d.Meta)
	}
	d.AppendVerified(Event{By: "human:xue", At: "2026-11-09T00:00:00Z", Result: "confirmed", Evidence: "read the code"})
	out, err := d.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"custom_key: keep me", "result: confirmed", "Body text.[^sess-1]", "by: human:xue"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	d2, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.Meta.Verified) != 2 || d2.Meta.Verified[1].Result != "confirmed" {
		t.Fatalf("reparse: %+v", d2.Meta.Verified)
	}
}

func TestNewStableOrder(t *testing.T) {
	d := New(Meta{
		Type: "Fact", Title: "x", Description: "y", Status: "draft",
		Resources: StringList{"pkg/**"},
		Generated: Event{By: "codex/gpt-5", At: "2026-11-08T00:00:00Z", Commit: "abc"},
	}, "hello")
	out, _ := d.Bytes()
	want := "---\ntype: Fact\ntitle: x\ndescription: y\nstatus: draft\nresources: [pkg/**]\ngenerated: {by: codex/gpt-5, at: \"2026-11-08T00:00:00Z\", commit: abc}\n---\nhello\n"
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse([]byte("no frontmatter")); err == nil {
		t.Fatal("want error")
	}
	if _, err := Parse([]byte("---\ntype: x\n")); err == nil {
		t.Fatal("want unterminated error")
	}
	d, err := Parse([]byte("---\n---\nbody"))
	if err != nil || d.Body != "body" {
		t.Fatalf("empty fm: %v %q", err, d.Body)
	}
	if _, err := Parse([]byte("---\ntags: x\nsupersedes: decisions/a\n---\n")); err != nil {
		t.Fatalf("scalar lists: %v", err)
	}
}
