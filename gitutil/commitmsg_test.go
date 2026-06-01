// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for commit message generation.

package gitutil

import (
	"strings"
	"testing"
)

func Test_extractPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
		want string
	}{
		{"standard", "diff --git a/foo/bar.go b/foo/bar.go", "foo/bar.go"},
		{"root file", "diff --git a/main.go b/main.go", "main.go"},
		{"deep path", "diff --git a/a/b/c/d.txt b/a/b/c/d.txt", "a/b/c/d.txt"},
		{"path with b/", "diff --git a/lib b/c.go b/lib b/c.go", "lib b/c.go"},
		{"space in path", "diff --git a/my file.go b/my file.go", "my file.go"},
		{"rename", "diff --git a/old.go b/new.go", "new.go"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractPath(tt.line)
			if got != tt.want {
				t.Errorf("extractPath(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func Test_isTestFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"go test", "foo/bar_test.go", true},
		{"python test", "tests/test_main.py", true},
		{"uppercase", "src/TestHelper.java", true},
		{"not test", "src/main.go", false},
		{"test in dir only", "test/main.go", false}, // basename is "main.go"
		{"testutil", "pkg/testutil.go", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isTestFile(tt.path)
			if got != tt.want {
				t.Errorf("isTestFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func Test_isDataFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"json", "data/config.json", true},
		{"yaml", "deploy/values.yaml", true},
		{"yml", "ci/.gitlab-ci.yml", true},
		{"go", "main.go", false},
		{"txt", "notes.txt", false},
		{"JSON upper", "DATA.JSON", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isDataFile(tt.path)
			if got != tt.want {
				t.Errorf("isDataFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func Test_isGeneratedFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"go.sum", "go.sum", true},
		{"nested go.sum", "submod/go.sum", true},
		{"package-lock", "package-lock.json", true},
		{"yarn.lock", "yarn.lock", true},
		{"pnpm-lock", "pnpm-lock.yaml", true},
		{"Cargo.lock", "Cargo.lock", true},
		{"Gemfile.lock", "Gemfile.lock", true},
		{"poetry.lock", "poetry.lock", true},
		{"composer.lock", "composer.lock", true},
		{"protobuf", "api/v1/service.pb.go", true},
		{"generated go", "internal/types_generated.go", true},
		{"vendor", "vendor/github.com/foo/bar.go", true},
		{"node_modules", "node_modules/lodash/index.js", true},
		{"normal go", "main.go", false},
		{"normal ts", "src/index.ts", false},
		{"lock in name", "locksmith.go", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isGeneratedFile(tt.path)
			if got != tt.want {
				t.Errorf("isGeneratedFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func Test_filterDiff(t *testing.T) {
	t.Parallel()
	diff := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1,3 +1,3 @@",
		" package main",
		"-// old",
		"+// new",
		"diff --git a/main_test.go b/main_test.go",
		"--- a/main_test.go",
		"+++ b/main_test.go",
		"@@ -1,3 +1,3 @@",
		" package main",
		"-// old test",
		"+// new test",
		"diff --git a/data.json b/data.json",
		"--- a/data.json",
		"+++ b/data.json",
		"@@ -1 +1 @@",
		`-{"a":1}`,
		`+{"a":2}`,
	}, "\n")

	t.Run("filter test files", func(t *testing.T) {
		t.Parallel()
		got := filterDiff(diff, isTestFile)
		if strings.Contains(got, "main_test.go") {
			t.Error("expected test file to be removed")
		}
		if !strings.Contains(got, "main.go") {
			t.Error("expected main.go to remain")
		}
		if !strings.Contains(got, "data.json") {
			t.Error("expected data.json to remain")
		}
	})

	t.Run("filter data files", func(t *testing.T) {
		t.Parallel()
		got := filterDiff(diff, isDataFile)
		if strings.Contains(got, "data.json") {
			t.Error("expected data file to be removed")
		}
		if !strings.Contains(got, "main.go") {
			t.Error("expected main.go to remain")
		}
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		got := filterDiff(diff, func(string) bool { return false })
		if got != diff {
			t.Error("expected no change when nothing excluded")
		}
	})

	t.Run("all match", func(t *testing.T) {
		t.Parallel()
		got := filterDiff(diff, func(string) bool { return true })
		// Should be empty or just newlines.
		if strings.Contains(got, "diff --git") {
			t.Error("expected all file sections removed")
		}
	})
}

func Test_reduceDiffContext(t *testing.T) {
	t.Parallel()
	t.Run("single hunk long context", func(t *testing.T) {
		t.Parallel()
		// Build a hunk with 10 context lines before and after a change.
		lines := make([]string, 0, 24)
		lines = append(lines,
			"diff --git a/f.go b/f.go",
			"--- a/f.go",
			"+++ b/f.go",
			"@@ -1,22 +1,22 @@",
		)
		for i := range 10 {
			lines = append(lines, " context line "+strings.Repeat("x", i))
		}
		lines = append(lines, "-old line", "+new line")
		for i := range 10 {
			lines = append(lines, " trailing context "+strings.Repeat("y", i))
		}

		got := reduceDiffContext(strings.Join(lines, "\n"))
		// Count context lines (lines starting with ' ').
		var ctxCount int
		for l := range strings.SplitSeq(got, "\n") {
			if strings.HasPrefix(l, " ") {
				ctxCount++
			}
		}
		if ctxCount > 6 {
			t.Errorf("expected at most 6 context lines, got %d", ctxCount)
		}
		if !strings.Contains(got, "-old line") || !strings.Contains(got, "+new line") {
			t.Error("change lines must be preserved")
		}
	})

	t.Run("already short", func(t *testing.T) {
		t.Parallel()
		diff := "diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1,5 +1,5 @@\n ctx1\n ctx2\n-old\n+new\n ctx3\n"
		got := reduceDiffContext(diff)
		// Should be unchanged since context is already <= 3.
		if got != diff {
			t.Errorf("expected no change for short context:\ngot:\n%s\nwant:\n%s", got, diff)
		}
	})

	t.Run("empty diff", func(t *testing.T) {
		t.Parallel()
		got := reduceDiffContext("")
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("multi file", func(t *testing.T) {
		t.Parallel()
		diff := strings.Join([]string{
			"diff --git a/a.go b/a.go",
			"--- a/a.go",
			"+++ b/a.go",
			"@@ -1,12 +1,12 @@",
			" c1", " c2", " c3", " c4", " c5",
			"-old",
			"+new",
			" c6", " c7", " c8", " c9", " c10",
			"diff --git a/b.go b/b.go",
			"--- a/b.go",
			"+++ b/b.go",
			"@@ -1,4 +1,4 @@",
			" x",
			"-old2",
			"+new2",
			" y",
		}, "\n")
		got := reduceDiffContext(diff)
		if !strings.Contains(got, "a.go") || !strings.Contains(got, "b.go") {
			t.Error("both files must be present")
		}
		if !strings.Contains(got, "-old") || !strings.Contains(got, "-old2") {
			t.Error("change lines must be preserved")
		}
	})
}

func Test_trimHunkContext(t *testing.T) {
	t.Parallel()
	t.Run("trim leading and trailing", func(t *testing.T) {
		t.Parallel()
		body := []string{
			" a", " b", " c", " d", " e",
			"-old",
			"+new",
			" f", " g", " h", " i", " j",
		}
		got, removed := trimHunkContext(body, 2)
		if removed == 0 {
			t.Error("expected some lines removed")
		}
		// Changed lines must survive.
		joined := strings.Join(got, "\n")
		if !strings.Contains(joined, "-old") || !strings.Contains(joined, "+new") {
			t.Error("change lines must be preserved")
		}
	})

	t.Run("empty body", func(t *testing.T) {
		t.Parallel()
		got, removed := trimHunkContext(nil, 3)
		if len(got) != 0 || removed != 0 {
			t.Errorf("expected empty result, got %v, %d", got, removed)
		}
	})

	t.Run("no context lines", func(t *testing.T) {
		t.Parallel()
		body := []string{"-a", "+b", "-c", "+d"}
		got, removed := trimHunkContext(body, 3)
		if removed != 0 {
			t.Errorf("expected 0 removed, got %d", removed)
		}
		if len(got) != 4 {
			t.Errorf("expected 4 lines, got %d", len(got))
		}
	})
}

func Test_splitDiff(t *testing.T) {
	t.Parallel()
	t.Run("single file", func(t *testing.T) {
		t.Parallel()
		diff := "diff --git a/f.go b/f.go\n-old\n+new"
		chunks := splitDiff(diff, 10000)
		if len(chunks) != 1 {
			t.Errorf("expected 1 chunk, got %d", len(chunks))
		}
	})

	t.Run("multi file grouping", func(t *testing.T) {
		t.Parallel()
		parts := make([]string, 0, 10)
		for i := range 5 {
			parts = append(parts,
				"diff --git a/f"+string(rune('0'+i))+".go b/f"+string(rune('0'+i))+".go",
				strings.Repeat("x", 100),
			)
		}
		diff := strings.Join(parts, "\n")
		// Each file section is ~110 bytes. With maxChunk=250, we should get multiple chunks.
		chunks := splitDiff(diff, 250)
		if len(chunks) < 2 {
			t.Errorf("expected multiple chunks, got %d", len(chunks))
		}
		// All chunks must contain diff headers.
		for i, c := range chunks {
			if !strings.Contains(c, "diff --git") {
				t.Errorf("chunk %d missing diff header", i)
			}
		}
	})

	t.Run("oversized single file", func(t *testing.T) {
		t.Parallel()
		diff := "diff --git a/huge.go b/huge.go\n" + strings.Repeat("x", 10000)
		chunks := splitDiff(diff, 100)
		if len(chunks) != 1 {
			t.Errorf("expected 1 chunk for oversized file, got %d", len(chunks))
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		chunks := splitDiff("", 1000)
		if len(chunks) != 0 {
			t.Errorf("expected 0 chunks, got %d", len(chunks))
		}
	})
}

func Test_filteredAnnotation(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		got := filteredAnnotation(nil)
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
	t.Run("single file", func(t *testing.T) {
		t.Parallel()
		got := filteredAnnotation([]string{"foo_test.go"})
		if !strings.Contains(got, "foo_test.go") {
			t.Error("expected file name in annotation")
		}
		if !strings.HasPrefix(got, "# [filtered:") {
			t.Errorf("expected '# [filtered:' prefix, got %q", got)
		}
		if !strings.HasSuffix(got, "\n") {
			t.Error("expected trailing newline")
		}
	})
	t.Run("multiple files", func(t *testing.T) {
		t.Parallel()
		got := filteredAnnotation([]string{"a_test.go", "b.yaml", "go.sum"})
		if !strings.Contains(got, "a_test.go") || !strings.Contains(got, "b.yaml") || !strings.Contains(got, "go.sum") {
			t.Errorf("expected all file names in annotation, got %q", got)
		}
	})
}

func Test_buildContext(t *testing.T) {
	t.Parallel()
	got := buildContext("meta\n", "diff\n")
	want := "meta\n=== Changes ===\ndiff\n"
	if got != want {
		t.Errorf("buildContext() = %q, want %q", got, want)
	}
}

func Test_parseDiff(t *testing.T) {
	t.Parallel()
	t.Run("single file single hunk", func(t *testing.T) {
		t.Parallel()
		diff := strings.Join([]string{
			"diff --git a/main.go b/main.go",
			"--- a/main.go",
			"+++ b/main.go",
			"@@ -1,3 +1,3 @@",
			" package main",
			"-// old",
			"+// new",
		}, "\n")
		files := parseDiff(diff)
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %d", len(files))
		}
		if files[0].path != "main.go" {
			t.Errorf("path = %q, want %q", files[0].path, "main.go")
		}
		if len(files[0].header) != 3 {
			t.Errorf("header lines = %d, want 3", len(files[0].header))
		}
		if len(files[0].hunks) != 1 {
			t.Fatalf("hunks = %d, want 1", len(files[0].hunks))
		}
		if !strings.HasPrefix(files[0].hunks[0].header, "@@") {
			t.Errorf("hunk header = %q, want @@ prefix", files[0].hunks[0].header)
		}
		if len(files[0].hunks[0].body) != 3 {
			t.Errorf("hunk body lines = %d, want 3", len(files[0].hunks[0].body))
		}
	})

	t.Run("multiple files", func(t *testing.T) {
		t.Parallel()
		diff := strings.Join([]string{
			"diff --git a/a.go b/a.go",
			"--- a/a.go",
			"+++ b/a.go",
			"@@ -1,2 +1,2 @@",
			"-old",
			"+new",
			"diff --git a/b.go b/b.go",
			"--- a/b.go",
			"+++ b/b.go",
			"@@ -1,2 +1,2 @@",
			"-old2",
			"+new2",
		}, "\n")
		files := parseDiff(diff)
		if len(files) != 2 {
			t.Fatalf("expected 2 files, got %d", len(files))
		}
		if files[0].path != "a.go" {
			t.Errorf("file[0].path = %q, want %q", files[0].path, "a.go")
		}
		if files[1].path != "b.go" {
			t.Errorf("file[1].path = %q, want %q", files[1].path, "b.go")
		}
	})

	t.Run("multiple hunks", func(t *testing.T) {
		t.Parallel()
		diff := strings.Join([]string{
			"diff --git a/f.go b/f.go",
			"--- a/f.go",
			"+++ b/f.go",
			"@@ -1,3 +1,3 @@",
			"-a",
			"+b",
			"@@ -10,3 +10,3 @@",
			"-c",
			"+d",
		}, "\n")
		files := parseDiff(diff)
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %d", len(files))
		}
		if len(files[0].hunks) != 2 {
			t.Errorf("hunks = %d, want 2", len(files[0].hunks))
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		files := parseDiff("")
		if len(files) != 0 {
			t.Errorf("expected 0 files, got %d", len(files))
		}
	})

	t.Run("index and mode lines in header", func(t *testing.T) {
		t.Parallel()
		diff := strings.Join([]string{
			"diff --git a/f.go b/f.go",
			"index abc1234..def5678 100644",
			"--- a/f.go",
			"+++ b/f.go",
			"@@ -1,2 +1,2 @@",
			"-old",
			"+new",
		}, "\n")
		files := parseDiff(diff)
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %d", len(files))
		}
		if len(files[0].header) != 4 {
			t.Errorf("header lines = %d, want 4 (diff, index, ---, +++)", len(files[0].header))
		}
	})
}

func TestRenderDiff(t *testing.T) {
	t.Parallel()
	tests := []string{
		// Single file, single hunk.
		strings.Join([]string{
			"diff --git a/main.go b/main.go",
			"--- a/main.go",
			"+++ b/main.go",
			"@@ -1,3 +1,3 @@",
			" package main",
			"-// old",
			"+// new",
		}, "\n"),
		// Multiple files.
		strings.Join([]string{
			"diff --git a/a.go b/a.go",
			"--- a/a.go",
			"+++ b/a.go",
			"@@ -1,2 +1,2 @@",
			"-old",
			"+new",
			"diff --git a/b.go b/b.go",
			"--- a/b.go",
			"+++ b/b.go",
			"@@ -1,2 +1,2 @@",
			"-old2",
			"+new2",
		}, "\n"),
		// Trailing newline.
		"diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1,5 +1,5 @@\n ctx1\n ctx2\n-old\n+new\n ctx3\n",
	}
	for i, diff := range tests {
		files := parseDiff(diff)
		got := renderDiff(files)
		if got != diff {
			t.Errorf("round-trip %d failed:\ngot:\n%s\nwant:\n%s", i, got, diff)
		}
	}
}

func TestRenderDiffLen(t *testing.T) {
	t.Parallel()
	diffs := []string{
		"diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1,2 +1,2 @@\n-old\n+new",
		strings.Join([]string{
			"diff --git a/a.go b/a.go",
			"--- a/a.go",
			"+++ b/a.go",
			"@@ -1,2 +1,2 @@",
			"-old",
			"+new",
			"diff --git a/b.go b/b.go",
			"--- a/b.go",
			"+++ b/b.go",
			"@@ -1,2 +1,2 @@",
			"-old2",
			"+new2",
		}, "\n"),
		"",
	}
	for i, diff := range diffs {
		files := parseDiff(diff)
		gotLen := renderDiffLen(files)
		wantLen := len(renderDiff(files))
		if gotLen != wantLen {
			t.Errorf("case %d: renderDiffLen = %d, len(renderDiff) = %d", i, gotLen, wantLen)
		}
	}
}

func TestReduceFileDiffContext(t *testing.T) {
	t.Parallel()
	diff := strings.Join([]string{
		"diff --git a/f.go b/f.go",
		"--- a/f.go",
		"+++ b/f.go",
		"@@ -1,22 +1,22 @@",
	}, "\n")
	// Add 10 context lines, a change, and 10 trailing context lines.
	lines := make([]string, 0, 22)
	for i := range 10 {
		lines = append(lines, " ctx"+strings.Repeat("x", i))
	}
	lines = append(lines, "-old", "+new")
	for i := range 10 {
		lines = append(lines, " trail"+strings.Repeat("y", i))
	}
	full := diff + "\n" + strings.Join(lines, "\n")
	files := parseDiff(full)
	reduceFileDiffContext(files, 3)

	var ctxCount int
	for _, line := range files[0].hunks[0].body {
		if strings.HasPrefix(line, " ") {
			ctxCount++
		}
	}
	if ctxCount > 6 {
		t.Errorf("expected at most 6 context lines, got %d", ctxCount)
	}
	// Changed lines must survive.
	rendered := renderDiff(files)
	if !strings.Contains(rendered, "-old") || !strings.Contains(rendered, "+new") {
		t.Error("change lines must be preserved")
	}
}

func Test_filterFiles(t *testing.T) {
	t.Parallel()
	diff := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1,2 +1,2 @@",
		"-old",
		"+new",
		"diff --git a/main_test.go b/main_test.go",
		"--- a/main_test.go",
		"+++ b/main_test.go",
		"@@ -1,2 +1,2 @@",
		"-old test",
		"+new test",
		"diff --git a/data.json b/data.json",
		"--- a/data.json",
		"+++ b/data.json",
		"@@ -1 +1 @@",
		`-{"a":1}`,
		`+{"a":2}`,
	}, "\n")
	files := parseDiff(diff)

	t.Run("exclude tests", func(t *testing.T) {
		t.Parallel()
		kept, removed := filterFiles(files, isTestFile)
		if len(kept) != 2 {
			t.Errorf("kept = %d, want 2", len(kept))
		}
		if len(removed) != 1 || removed[0] != "main_test.go" {
			t.Errorf("removed = %v, want [main_test.go]", removed)
		}
	})

	t.Run("exclude none", func(t *testing.T) {
		t.Parallel()
		kept, removed := filterFiles(files, func(string) bool { return false })
		if len(kept) != 3 {
			t.Errorf("kept = %d, want 3", len(kept))
		}
		if len(removed) != 0 {
			t.Errorf("removed = %v, want []", removed)
		}
	})

	t.Run("exclude all", func(t *testing.T) {
		t.Parallel()
		kept, removed := filterFiles(files, func(string) bool { return true })
		if len(kept) != 0 {
			t.Errorf("kept = %d, want 0", len(kept))
		}
		if len(removed) != 3 {
			t.Errorf("removed count = %d, want 3", len(removed))
		}
	})
}

func TestProgressiveFilter(t *testing.T) {
	t.Parallel()
	makeTestOnlyDiff := func() []fileDiff {
		return parseDiff(strings.Join([]string{
			"diff --git a/foo_test.go b/foo_test.go",
			"--- a/foo_test.go",
			"+++ b/foo_test.go",
			"@@ -1,2 +1,2 @@",
			"-old",
			"+new",
			"diff --git a/bar_test.go b/bar_test.go",
			"--- a/bar_test.go",
			"+++ b/bar_test.go",
			"@@ -1,2 +1,2 @@",
			"-old",
			"+new",
		}, "\n"))
	}

	t.Run("skip filter when all files would be removed", func(t *testing.T) {
		t.Parallel()
		files := makeTestOnlyDiff()
		// A very small budget forces the filter to be applied.
		kept, removed := progressiveFilter(files, []func(string) bool{isTestFile}, 0)
		if len(kept) == 0 {
			t.Error("kept is empty: filter should have been skipped when all files match")
		}
		if len(removed) != 0 {
			t.Errorf("removed = %v, want [] (filter skipped)", removed)
		}
	})

	t.Run("filter applied when some files remain", func(t *testing.T) {
		t.Parallel()
		files := parseDiff(strings.Join([]string{
			"diff --git a/main.go b/main.go",
			"--- a/main.go",
			"+++ b/main.go",
			"@@ -1,2 +1,2 @@",
			strings.Repeat("-x", 200),
			strings.Repeat("+y", 200),
			"diff --git a/main_test.go b/main_test.go",
			"--- a/main_test.go",
			"+++ b/main_test.go",
			"@@ -1,2 +1,2 @@",
			strings.Repeat("-x", 200),
			strings.Repeat("+y", 200),
		}, "\n"))
		// Budget so small it forces filtering.
		kept, removed := progressiveFilter(files, []func(string) bool{isTestFile}, 0)
		if len(kept) != 1 || kept[0].path != "main.go" {
			t.Errorf("kept = %v, want [main.go]", kept)
		}
		if len(removed) != 1 || removed[0] != "main_test.go" {
			t.Errorf("removed = %v, want [main_test.go]", removed)
		}
	})

	t.Run("no filter needed when diff fits budget", func(t *testing.T) {
		t.Parallel()
		files := makeTestOnlyDiff()
		kept, removed := progressiveFilter(files, []func(string) bool{isTestFile}, 1_000_000)
		if len(kept) != len(files) {
			t.Errorf("kept = %d, want %d (no filtering needed)", len(kept), len(files))
		}
		if len(removed) != 0 {
			t.Errorf("removed = %v, want []", removed)
		}
	})
}

func TestSplitFiles(t *testing.T) {
	t.Parallel()
	t.Run("single chunk", func(t *testing.T) {
		t.Parallel()
		files := parseDiff(strings.Join([]string{
			"diff --git a/f.go b/f.go",
			"--- a/f.go",
			"+++ b/f.go",
			"@@ -1,2 +1,2 @@",
			"-old",
			"+new",
		}, "\n"))
		chunks := splitFiles(files, 10000)
		if len(chunks) != 1 {
			t.Errorf("expected 1 chunk, got %d", len(chunks))
		}
	})

	t.Run("multiple chunks", func(t *testing.T) {
		t.Parallel()
		parts := make([]string, 0, 10)
		for i := range 5 {
			parts = append(parts,
				"diff --git a/f"+string(rune('0'+i))+".go b/f"+string(rune('0'+i))+".go",
				strings.Repeat("x", 100),
			)
		}
		files := parseDiff(strings.Join(parts, "\n"))
		chunks := splitFiles(files, 250)
		if len(chunks) < 2 {
			t.Errorf("expected multiple chunks, got %d", len(chunks))
		}
		for i, c := range chunks {
			if !strings.Contains(c, "diff --git") {
				t.Errorf("chunk %d missing diff header", i)
			}
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		chunks := splitFiles(nil, 1000)
		if len(chunks) != 0 {
			t.Errorf("expected 0 chunks, got %d", len(chunks))
		}
	})

	t.Run("directory grouping", func(t *testing.T) {
		t.Parallel()
		// Files from the same directory should end up in the same chunk
		// even if the input order is interleaved.
		diff := strings.Join([]string{
			"diff --git a/pkg/z.go b/pkg/z.go",
			"@@ -1 +1 @@",
			"-z",
			"+zz",
			"diff --git a/cmd/a.go b/cmd/a.go",
			"@@ -1 +1 @@",
			"-a",
			"+aa",
			"diff --git a/pkg/a.go b/pkg/a.go",
			"@@ -1 +1 @@",
			"-a",
			"+aa",
			"diff --git a/cmd/z.go b/cmd/z.go",
			"@@ -1 +1 @@",
			"-z",
			"+zz",
		}, "\n")
		files := parseDiff(diff)
		// Use a small chunk size so each chunk gets at most 2 files.
		// Each file ~52 bytes, two files ~105 bytes.
		chunks := splitFiles(files, 120)
		// Both cmd/ files should be in the same chunk.
		for _, c := range chunks {
			if strings.Contains(c, "cmd/a.go") {
				if !strings.Contains(c, "cmd/z.go") {
					t.Error("expected cmd/a.go and cmd/z.go in the same chunk")
				}
			}
			if strings.Contains(c, "pkg/a.go") {
				if !strings.Contains(c, "pkg/z.go") {
					t.Error("expected pkg/a.go and pkg/z.go in the same chunk")
				}
			}
		}
	})
}
