// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for commit message generation.

package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/maruel/genai"
)

func TestGenerateCommitMsg(t *testing.T) { //nolint:tparallel // sibling subtests set process environment.
	t.Run("whole diff", func(t *testing.T) {
		t.Setenv("GIT_DESC_TOKENS", "8000")
		p := &recordingProvider{reply: func(prompt string) (string, error) { return "Describe change", nil }}
		got, err := GenerateCommitMsg(t.Context(), p, "=== Branch ===\nmain\n\n", testDiff("a.go", "+new\n"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != "Describe change" {
			t.Fatalf("GenerateCommitMsg() = %q, want Describe change", got)
		}
		calls := p.snapshot()
		if len(calls) != 1 || calls[0].prompt != commitMsgPrompt || !strings.Contains(calls[0].content, "+new") {
			t.Fatalf("calls = %+v, want one whole-diff request", calls)
		}
	})

	t.Run("splits oversized hunks within budget", func(t *testing.T) {
		t.Setenv("GIT_DESC_TOKENS", "8000")
		var body strings.Builder
		for i := range 8_000 {
			fmt.Fprintf(&body, "+line %04d changed value\n", i)
		}
		p := &recordingProvider{reply: func(prompt string) (string, error) {
			if prompt == finalPrompt {
				return "Describe large change", nil
			}
			return "part summary", nil
		}}
		var progress strings.Builder
		metadata := "=== Branch ===\nmain\n\n=== Recent Commits ===\nvery long history\n"
		got, err := GenerateCommitMsg(t.Context(), p, metadata, testDiff("huge.go", body.String()), &CommitMsgOptions{ContextTokens: 8_000, Progress: &progress})
		if err != nil {
			t.Fatal(err)
		}
		if got != "Describe large change" {
			t.Fatalf("GenerateCommitMsg() = %q", got)
		}
		budget, _ := contextBudget(8_000)
		calls := p.snapshot()
		partCalls := 0
		for _, call := range calls {
			if len(call.content) > budget {
				t.Errorf("request length = %d, budget = %d", len(call.content), budget)
			}
			if call.prompt == partPrompt {
				partCalls++
				if strings.Contains(call.content, "very long history") {
					t.Error("part request contains full recent-commit metadata")
				}
			}
		}
		if partCalls < 2 || calls[len(calls)-1].prompt != finalPrompt {
			t.Fatalf("part calls = %d, final prompt = %q", partCalls, calls[len(calls)-1].prompt)
		}
		if !strings.Contains(progress.String(), "splitting into") {
			t.Fatalf("progress = %q, want split notice", progress.String())
		}
	})

	t.Run("splits oversized summary before merging", func(t *testing.T) {
		t.Parallel()
		p := &recordingProvider{reply: func(prompt string) (string, error) {
			switch prompt {
			case partPrompt:
				return strings.Repeat("large summary ", 3_000), nil
			case mergePrompt:
				return "merged", nil
			default:
				return "Describe large change", nil
			}
		}}
		diff := testDiff("huge.go", strings.Repeat("+changed value\n", 20_000))
		_, err := GenerateCommitMsg(t.Context(), p, "metadata\n", diff, &CommitMsgOptions{ContextTokens: 8_000})
		if err != nil {
			t.Fatal(err)
		}
		budget, _ := contextBudget(8_000)
		for _, call := range p.snapshot() {
			if len(call.content) > budget {
				t.Errorf("request length = %d, budget = %d", len(call.content), budget)
			}
		}
	})

	t.Run("propagates part failure", func(t *testing.T) {
		t.Setenv("GIT_DESC_TOKENS", "8000")
		p := &recordingProvider{reply: func(prompt string) (string, error) {
			if prompt == partPrompt {
				return "", errors.New("provider failed")
			}
			return "unexpected", nil
		}}
		diff := testDiff("huge.go", strings.Repeat("+changed value\n", 20_000))
		_, err := GenerateCommitMsg(t.Context(), p, "metadata\n", diff, nil)
		if err == nil || !strings.Contains(err.Error(), "provider failed") {
			t.Fatalf("GenerateCommitMsg() error = %v, want provider failure", err)
		}
	})

	t.Run("validates token budget", func(t *testing.T) {
		t.Setenv("GIT_DESC_TOKENS", "7999")
		_, err := GenerateCommitMsg(t.Context(), &recordingProvider{}, "", testDiff("a.go", "+a\n"), nil)
		if err == nil || !strings.Contains(err.Error(), "at least 8000") {
			t.Fatalf("GenerateCommitMsg() error = %v, want minimum-token error", err)
		}
	})
}

func TestDiffReduction(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/a.go b/a.go\nindex 1..2 100644\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@ fn\n old\n-old\n+new\n" +
		testDiff("go.sum", "+checksum\n") +
		testDiff("gone.go", "-deleted\n")
	diff = strings.Replace(diff, "diff --git a/gone.go b/gone.go\n", "diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n", 1)
	files, err := parseDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("parseDiff() returned %d files", len(files))
	}
	rendered := renderFiles(files)
	for _, unwanted := range []string{"index 1..2", "--- a/a.go", "+++ b/a.go", "+checksum", "-deleted"} {
		if strings.Contains(rendered, unwanted) {
			t.Errorf("rendered diff contains %q", unwanted)
		}
	}
	if strings.Count(rendered, "(content omitted)") != 2 {
		t.Errorf("rendered diff = %q, want two omitted bodies", rendered)
	}
}

func TestGitPathsAndUTF8(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/lib b/name.go b/lib b/name.go\n--- a/lib b/name.go\n+++ b/lib b/name.go\n@@ -1 +1 @@\n+x\n"
	files, err := parseDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if files[0].path != "lib b/name.go" {
		t.Errorf("path = %q, want %q", files[0].path, "lib b/name.go")
	}
	quoted := "diff --git \"a/caf\\303\\251.go\" \"b/caf\\303\\251.go\"\n+++ \"b/caf\\303\\251.go\"\n@@ -1 +1 @@\n+x\n"
	files, err = parseDiff(quoted)
	if err != nil {
		t.Fatal(err)
	}
	if files[0].path != "café.go" {
		t.Errorf("quoted path = %q, want café.go", files[0].path)
	}
	quotedBinary := "diff --git \"a/my file.bin\" \"b/my file.bin\"\nBinary files differ\n"
	files, err = parseDiff(quotedBinary)
	if err != nil {
		t.Fatal(err)
	}
	if files[0].path != "my file.bin" {
		t.Errorf("quoted binary path = %q, want my file.bin", files[0].path)
	}
	clipped := clipDiffLine(strings.Repeat("é", maxDiffLine))
	if !strings.Contains(clipped, "[truncated]") || !utf8.ValidString(clipped) {
		t.Errorf("clipped line is not valid truncated UTF-8: %q", clipped)
	}
}

func TestPack(t *testing.T) {
	t.Parallel()
	groups := pack([]string{strings.Repeat("x", 10), strings.Repeat("x", 10), strings.Repeat("x", 10), strings.Repeat("x", 10)}, 25)
	if len(groups) != 2 || len(groups[0]) != 2 || len(groups[1]) != 2 {
		t.Fatalf("pack() = %#v, want two balanced groups", groups)
	}
}

func testDiff(name, body string) string {
	return "diff --git a/" + name + " b/" + name + "\n@@ -1,0 +1,1 @@\n" + body
}

type providerCall struct {
	prompt  string
	content string
}

type recordingProvider struct {
	genai.Provider

	mu    sync.Mutex
	calls []providerCall
	reply func(string) (string, error)
}

func (p *recordingProvider) GenSync(_ context.Context, msgs genai.Messages, opts ...genai.GenOption) (genai.Result, error) {
	textOpts, ok := opts[0].(*genai.GenOptionText)
	if !ok {
		return genai.Result{}, fmt.Errorf("option type = %T, want *genai.GenOptionText", opts[0])
	}
	content := msgs[0].String()
	p.mu.Lock()
	p.calls = append(p.calls, providerCall{prompt: textOpts.SystemPrompt, content: content})
	p.mu.Unlock()
	if p.reply == nil {
		return genai.Result{}, errors.New("unexpected provider call")
	}
	text, err := p.reply(textOpts.SystemPrompt)
	return genai.Result{Message: genai.NewTextMessage(text)}, err
}

func (p *recordingProvider) snapshot() []providerCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providerCall(nil), p.calls...)
}
