// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package gitutil

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/maruel/genai"
	"golang.org/x/sync/errgroup"
)

const (
	maxDiffLen       = 200_000
	reducedContext   = 3
	maxParallelCalls = 4
)

// commitMsgPrompt is the system prompt used by GenerateCommitMsg for direct
// commit message generation from a diff.
const commitMsgPrompt = "Write a git commit message for the changes below. Follow these rules:\n" +
	"- Subject: imperative mood, no period, max 72 chars (e.g. \"Fix timeout in retry loop\")\n" +
	"- If the change is non-trivial, add a blank line then a body explaining what and why, not how\n" +
	"- Wrap body lines at 72 chars\n" +
	"- Match the style of recent upstream commits if provided\n" +
	"- Focus on the meaningful changes; ignore ancillary updates (imports, test data, build files, dependency bumps, formatting) unless they are the primary purpose of the commit\n" +
	"- No emojis\n" +
	"- Output only the commit message, nothing else"

// chunkPrompt is the system prompt used to summarize individual diff chunks
// during parallel map-reduce for large diffs.
const chunkPrompt = "Summarize the following diff chunk concisely. Focus on what changed and why. Keep it brief (2-5 sentences)."

// synthesizePrompt is the system prompt used to combine chunk summaries into
// a final commit message during parallel map-reduce for large diffs.
const synthesizePrompt = "Below are descriptions of different parts of the same commit. " +
	"Write a single unified git commit message following these rules:\n" +
	"- Subject: imperative mood, no period, max 72 chars\n" +
	"- If non-trivial, add a blank line then a body explaining what and why\n" +
	"- Wrap body lines at 72 chars\n" +
	"- Match the style of recent upstream commits if provided\n" +
	"- No emojis\n" +
	"- Output only the commit message, nothing else"

// defaultDiffFilters is the default sequence of file predicates applied
// progressively by GenerateCommitMsg when a diff exceeds the context limit.
// Each filter is tried in order; matching files are removed only if the diff
// is still too large after the previous step. Pass nil to GenerateCommitMsg
// to use these defaults.
var defaultDiffFilters = []func(string) bool{isTestFile, isDataFile, isGeneratedFile}

// hunk represents a single hunk in a unified diff.
type hunk struct {
	header string   // the @@ line
	body   []string // lines after the header
}

// fileDiff represents a single file's diff section.
type fileDiff struct {
	path   string   // file path extracted from "diff --git" header
	header []string // "diff --git", "index", "---", "+++" lines
	hunks  []hunk
}

// parseDiff parses a unified diff string into structured fileDiff values.
func parseDiff(diff string) []fileDiff {
	if diff == "" {
		return nil
	}
	lines := strings.Split(diff, "\n")
	var files []fileDiff
	cur := -1
	inHunk := false
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git") {
			files = append(files, fileDiff{path: extractPath(line)})
			cur = len(files) - 1
			inHunk = false
			files[cur].header = append(files[cur].header, line)
			continue
		}
		if cur < 0 {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			files[cur].hunks = append(files[cur].hunks, hunk{header: line})
			inHunk = true
			continue
		}
		if inHunk {
			h := &files[cur].hunks[len(files[cur].hunks)-1]
			h.body = append(h.body, line)
		} else {
			files[cur].header = append(files[cur].header, line)
		}
	}
	return files
}

// renderDiff serializes parsed file diffs back into a unified diff string.
func renderDiff(files []fileDiff) string {
	n := 0
	for _, f := range files {
		n += len(f.header)
		for _, h := range f.hunks {
			n += 1 + len(h.body)
		}
	}
	lines := make([]string, 0, n)
	for _, f := range files {
		lines = append(lines, f.header...)
		for _, h := range f.hunks {
			lines = append(lines, h.header)
			lines = append(lines, h.body...)
		}
	}
	return strings.Join(lines, "\n")
}

// fileDiffLen returns the rendered length of a single fileDiff.
func fileDiffLen(f *fileDiff) int {
	n := 0
	numLines := 0
	for _, line := range f.header {
		n += len(line)
		numLines++
	}
	for _, h := range f.hunks {
		n += len(h.header)
		numLines++
		for _, line := range h.body {
			n += len(line)
			numLines++
		}
	}
	if numLines == 0 {
		return 0
	}
	return n + numLines - 1
}

// renderDiffLen returns the length of the string that renderDiff would produce
// without allocating it.
func renderDiffLen(files []fileDiff) int {
	if len(files) == 0 {
		return 0
	}
	n := len(files) - 1 // \n separators between files
	for i := range files {
		n += fileDiffLen(&files[i])
	}
	return n
}

// extractPath parses the filename from a "diff --git a/path b/path" header line.
func extractPath(diffLine string) string {
	// Format: "diff --git a/X b/X" where both paths are identical (non-rename).
	const prefix = "diff --git a/"
	if !strings.HasPrefix(diffLine, prefix) {
		return ""
	}
	rest := diffLine[len(prefix):] // "X b/X"
	// For identical paths: rest = path + " b/" + path, so len = 2*pathLen + 3.
	if (len(rest)-3)%2 == 0 {
		pathLen := (len(rest) - 3) / 2
		sep := pathLen // expected position of " b/" in rest
		if sep >= 0 && sep+3 <= len(rest) && rest[sep:sep+3] == " b/" && rest[:pathLen] == rest[sep+3:] {
			return rest[:pathLen]
		}
	}
	// Fallback for renames (a/old b/new) where paths differ.
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return ""
}

// isTestFile returns true if the basename contains "test" (case-insensitive).
func isTestFile(name string) bool {
	return strings.Contains(strings.ToLower(path.Base(name)), "test")
}

// isDataFile returns true for .json, .yaml, and .yml files.
func isDataFile(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".json" || ext == ".yaml" || ext == ".yml"
}

// isGeneratedFile returns true for lock files, generated code, and vendored
// dependencies.
func isGeneratedFile(name string) bool {
	lower := strings.ToLower(name)
	switch strings.ToLower(path.Base(name)) {
	case "cargo.lock", "composer.lock", "gemfile.lock", "go.sum",
		"package-lock.json", "pnpm-lock.yaml", "poetry.lock", "yarn.lock":
		return true
	}
	if strings.HasSuffix(lower, ".pb.go") || strings.HasSuffix(lower, "_generated.go") {
		return true
	}
	for part := range strings.SplitSeq(lower, "/") {
		if part == "vendor" || part == "node_modules" {
			return true
		}
	}
	for part := range strings.SplitSeq(lower, "\\") {
		if part == "vendor" || part == "node_modules" {
			return true
		}
	}
	return false
}

// buildContext concatenates metadata and diff with a separator.
func buildContext(metadata, diff string) string {
	return metadata + "=== Changes ===\n" + diff
}

// filteredAnnotation returns a comment line listing the files that were
// omitted from the diff so the LLM knows they existed.
func filteredAnnotation(removed []string) string {
	if len(removed) == 0 {
		return ""
	}
	return "# [filtered: " + strings.Join(removed, ", ") + " — omitted to fit context]\n"
}

// reduceFileDiffContext trims context lines in each hunk to at most target
// lines before and after changed lines.
func reduceFileDiffContext(files []fileDiff, target int) {
	for i := range files {
		for j := range files[i].hunks {
			files[i].hunks[j].body, _ = trimHunkContext(files[i].hunks[j].body, target)
		}
	}
}

// reduceDiffContext rewrites a unified diff, trimming context lines in each
// hunk to at most reducedContext lines before and after changed lines.
func reduceDiffContext(diff string) string {
	files := parseDiff(diff)
	reduceFileDiffContext(files, reducedContext)
	return renderDiff(files)
}

// hunkSpan marks a contiguous run of context or changed lines in a hunk body.
type hunkSpan struct {
	start, end int
	context    bool
}

// trimHunkContext trims leading and trailing context-only runs and
// inter-change context runs to at most target lines on each side.
//
// Returns the trimmed lines and the number of lines removed.
func trimHunkContext(body []string, target int) (out []string, removedCount int) {
	if len(body) == 0 {
		return body, 0
	}

	// Identify which lines are context (start with ' ' or are empty context).
	isCtx := func(line string) bool {
		return line == "" || line[0] == ' '
	}

	// Find runs of context lines and changed lines.
	var spans []hunkSpan
	i := 0
	for i < len(body) {
		ctx := isCtx(body[i])
		j := i + 1
		for j < len(body) && isCtx(body[j]) == ctx {
			j++
		}
		spans = append(spans, hunkSpan{i, j, ctx})
		i = j
	}

	var removed int
	for si, s := range spans {
		if !s.context {
			out = append(out, body[s.start:s.end]...)
			continue
		}
		runLen := s.end - s.start
		if runLen <= target*2 {
			// Short enough, keep all.
			out = append(out, body[s.start:s.end]...)
			continue
		}
		// Leading context (first span or after a changed span).
		// Trailing context (last span or before a changed span).
		keepEnd := target   // trailing lines from this run (context before next change)
		keepStart := target // leading lines from this run (context after prev change)
		if si == 0 {
			keepStart = target
			keepEnd = 0
		}
		if si == len(spans)-1 {
			keepStart = 0
			keepEnd = target
		}
		if keepStart+keepEnd >= runLen {
			out = append(out, body[s.start:s.end]...)
			continue
		}
		if keepStart > 0 {
			out = append(out, body[s.start:s.start+keepStart]...)
		}
		if keepEnd > 0 {
			out = append(out, body[s.end-keepEnd:s.end]...)
		}
		removed += runLen - keepStart - keepEnd
	}
	return out, removed
}

// filterFiles partitions files into kept and removed based on the exclude
// predicate.
func filterFiles(files []fileDiff, exclude func(string) bool) (kept []fileDiff, removed []string) {
	for _, f := range files {
		if exclude(f.path) {
			removed = append(removed, f.path)
		} else {
			kept = append(kept, f)
		}
	}
	return kept, removed
}

// filterDiff removes file sections from a unified diff where exclude returns
// true for the file path.
func filterDiff(diff string, exclude func(string) bool) string {
	files := parseDiff(diff)
	kept, _ := filterFiles(files, exclude)
	return renderDiff(kept)
}

// splitFiles splits file diffs into chunks that each fit under maxChunk bytes.
// A single file that exceeds maxChunk is returned as its own chunk. Files are
// sorted by path so that files in the same directory land in the same chunk.
func splitFiles(files []fileDiff, maxChunk int) []string {
	if len(files) == 0 {
		return nil
	}
	sorted := make([]fileDiff, len(files))
	copy(sorted, files)
	slices.SortFunc(sorted, func(a, b fileDiff) int {
		return cmp.Compare(a.path, b.path)
	})
	files = sorted
	var chunks []string
	var chunk []fileDiff
	chunkLen := 0
	for i := range files {
		fLen := fileDiffLen(&files[i])
		if chunkLen > 0 && chunkLen+1+fLen > maxChunk {
			chunks = append(chunks, renderDiff(chunk))
			chunk = nil
			chunkLen = 0
		}
		chunk = append(chunk, files[i])
		if chunkLen == 0 {
			chunkLen = fLen
		} else {
			chunkLen += 1 + fLen
		}
	}
	if len(chunk) > 0 {
		chunks = append(chunks, renderDiff(chunk))
	}
	return chunks
}

// splitDiff splits a unified diff at "diff --git" boundaries into chunks
// that each fit under maxChunk bytes. A single file that exceeds maxChunk is
// returned as its own chunk.
func splitDiff(diff string, maxChunk int) []string {
	files := parseDiff(diff)
	if len(files) == 0 {
		if diff != "" {
			return []string{diff}
		}
		return nil
	}
	return splitFiles(files, maxChunk)
}

// progressiveFilter applies filters in order to reduce files until
// renderDiffLen(result) + len(filteredAnnotation(removed)) fits within budget.
// If a filter would eliminate all remaining files, it is skipped to ensure
// there is always something to describe. Returns the kept files and all
// removed file paths accumulated across applied filters.
func progressiveFilter(files []fileDiff, filters []func(string) bool, budget int) (kept []fileDiff, removedPaths []string) {
	var removed []string
	for _, f := range filters {
		kept, r := filterFiles(files, f)
		if len(kept) == 0 {
			// Skip: applying this filter would leave nothing to describe.
			continue
		}
		files = kept
		removed = append(removed, r...)
		if renderDiffLen(files)+len(filteredAnnotation(removed)) <= budget {
			break
		}
	}
	return files, removed
}

// GenerateCommitMsg applies a progressive reduction pipeline to fit the diff
// under the LLM context limit, then calls the LLM to produce a commit message.
//
// metadata should contain git context (branch name, file stats, recent commit
// messages). diff should be a unified diff of the changes to describe.
// filters is an ordered list of file predicates applied progressively to
// reduce the diff size. Pass nil to use defaultDiffFilters.
func GenerateCommitMsg(ctx context.Context, p genai.Provider, metadata, diff string, filters []func(string) bool) (string, error) {
	if filters == nil {
		filters = defaultDiffFilters
	}
	files := parseDiff(diff)
	metaLen := len(metadata) + len("=== Changes ===\n")

	// Step 0: try full diff.
	if metaLen+renderDiffLen(files) <= maxDiffLen {
		return genCommitMsg(ctx, p, commitMsgPrompt, buildContext(metadata, renderDiff(files)))
	}

	// Step 1: reduce context lines.
	reduceFileDiffContext(files, reducedContext)
	if metaLen+renderDiffLen(files) <= maxDiffLen {
		return genCommitMsg(ctx, p, commitMsgPrompt, buildContext(metadata, renderDiff(files)))
	}

	// Step 2+: apply each filter progressively until the diff fits.
	files, removed := progressiveFilter(files, filters, maxDiffLen-metaLen)
	annotation := filteredAnnotation(removed)
	if metaLen+renderDiffLen(files)+len(annotation) <= maxDiffLen {
		return genCommitMsg(ctx, p, commitMsgPrompt, buildContext(metadata, renderDiff(files)+annotation))
	}

	// Final fallback: parallel map-reduce. Include annotation in metadata so
	// the synthesis step knows which files were omitted.
	return parallelDescribe(ctx, p, metadata+annotation, files)
}

const maxMetadataPrefix = 10000

// parallelDescribe splits the diff into chunks, summarizes each concurrently,
// then synthesizes the summaries into a single commit message. Each chunk
// prompt includes a truncated metadata header for context.
func parallelDescribe(ctx context.Context, p genai.Provider, metadata string, files []fileDiff) (string, error) {
	// Truncate metadata prefix for chunk prompts to avoid blowing the budget.
	metaPrefix := metadata
	if len(metaPrefix) > maxMetadataPrefix {
		metaPrefix = metaPrefix[:maxMetadataPrefix] + "\n...[truncated]\n"
	}
	chunkOverhead := len(chunkPrompt) + len("\n\n") + len(metaPrefix) + len("\n") + 100
	chunkSize := maxDiffLen - chunkOverhead
	chunkSize = max(chunkSize, 1000)
	chunks := splitFiles(files, chunkSize)
	if len(chunks) == 0 {
		return genCommitMsg(ctx, p, commitMsgPrompt, metadata)
	}

	summaries := make([]string, len(chunks))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxParallelCalls)
	for i, chunk := range chunks {
		g.Go(func() error {
			header := fmt.Sprintf("(part %d/%d)\n", i+1, len(chunks))
			content := metaPrefix + "\n" + header + chunk
			summary, err := genCommitMsg(gctx, p, chunkPrompt, content)
			if err != nil {
				return err
			}
			summaries[i] = summary
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return "", err
	}

	// Synthesize.
	combined := metadata + "\n=== Chunk Summaries ===\n" + strings.Join(summaries, "\n---\n")
	return genCommitMsg(ctx, p, synthesizePrompt, combined)
}

// genCommitMsg generates a commit message using an already-initialized provider.
//
// The system prompt contains instructions; the user content contains the diff
// and metadata. Separating them lets the LLM weight instructions correctly.
func genCommitMsg(ctx context.Context, p genai.Provider, systemPrompt, content string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := p.GenSync(ctx, genai.Messages{genai.NewTextMessage(content)}, &genai.GenOptionText{
		MaxTokens:    1024,
		SystemPrompt: systemPrompt,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.String()), nil
}
