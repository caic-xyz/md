// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Commit message generation and formatting.

package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/maruel/genai"
	"golang.org/x/sync/errgroup"
)

const (
	// DefaultCommitMessageTokens is the assumed model context window.
	DefaultCommitMessageTokens = 64_000
	// MinCommitMessageTokens is the smallest supported model context window.
	MinCommitMessageTokens = 8_000

	charsPerToken    = 3
	reservedShare    = 0.25
	reducedContext   = 3
	maxDiffLine      = 1_000
	maxParallelCalls = 4
	requestTimeout   = 5 * time.Minute
)

const commitMsgRules = "- Subject: imperative mood, no period, max 120 chars (e.g. \"Fix timeout in retry loop\")\n" +
	"- Default to a body; omit it only for an obviously trivial, self-explanatory change such as a typo, formatting-only change, or comment-only change\n" +
	"- Changes that affect behavior, interfaces, dependencies, data, architecture, or multiple meaningful concerns require a body after a blank line\n" +
	"- Explain the rationale and consequential tradeoffs; do not merely restate the subject or input\n" +
	"- Wrap body lines at 80 columns\n" +
	"- Match the style of recent upstream commits only when it does not conflict with these rules\n" +
	"- Focus on the meaningful changes; ignore ancillary updates (imports, tests, build files, dependency bumps, formatting) unless they are the primary purpose of the change\n" +
	"- No emojis\n" +
	"- Output only the commit message, nothing else"

const commitMsgPrompt = "Write a git commit message for the change below. Follow these rules:\n" + commitMsgRules

const partPrompt = "The input is one part of a larger change. Summarize what it changes and why in a few short paragraphs. Output only the summary."

const finalPrompt = "Below are metadata and summaries of the parts of one change. " +
	"Write a single git commit message for the change. Follow these rules:\n" + commitMsgRules

const mergePrompt = "Below are summaries of parts of one change. Merge them into one summary. Keep every distinct point and drop repetition. Output only the summary."

var hunkPattern = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@(.*)$`)

var defaultDiffFilters = []func(string) bool{isTestFile, isDataFile}

// CommitMsgOptions configures [GenerateCommitMsg].
type CommitMsgOptions struct {
	// BriefMetadata replaces metadata in requests that summarize one diff part.
	// When empty, GenerateCommitMsg removes the recent-commit section from metadata.
	BriefMetadata string
	// ContextTokens is the model context window. Zero reads GIT_DESC_TOKENS and
	// then defaults to [DefaultCommitMessageTokens].
	ContextTokens int
	// Filters are applied progressively to omit low-value file bodies. Nil uses
	// the default test and structured-data filters.
	Filters []func(string) bool
	// Progress receives notices when a diff is reduced, split, or merged.
	Progress io.Writer
}

type hunk struct {
	oldStart int
	newStart int
	section  string
	lines    []string
}

func (h *hunk) render() string {
	oldCount, newCount := lineCounts(h.lines)
	header := fmt.Sprintf("@@ -%d,%d +%d,%d @@%s\n", h.oldStart, oldCount, h.newStart, newCount, h.section)
	return header + strings.Join(h.lines, "")
}

func (h *hunk) withContext(n int) hunk {
	keep := make([]bool, len(h.lines))
	for i := range keep {
		keep[i] = true
	}
	lead := 0
	for i := 0; i < len(h.lines); {
		if !strings.HasPrefix(h.lines[i], " ") {
			i++
			continue
		}
		start := i
		for i < len(h.lines) && strings.HasPrefix(h.lines[i], " ") {
			i++
		}
		dropStart := 0
		var dropEnd int
		switch {
		case start == 0:
			dropEnd = max(0, i-n)
			lead = dropEnd
		case i == len(h.lines):
			dropStart, dropEnd = min(start+n, i), i
		default:
			dropStart, dropEnd = min(start+n, i), max(start, i-n)
		}
		for j := dropStart; j < dropEnd; j++ {
			keep[j] = false
		}
	}
	lines := make([]string, 0, len(h.lines)-lead)
	for i, line := range h.lines {
		if keep[i] {
			lines = append(lines, line)
		}
	}
	return hunk{oldStart: h.oldStart + lead, newStart: h.newStart + lead, section: h.section, lines: lines}
}

func (h *hunk) split(limit int) []hunk {
	room := limit - len(h.render()) + len(strings.Join(h.lines, "")) - 8
	groups := pack(h.lines, room)
	result := make([]hunk, 0, len(groups))
	oldStart, newStart := h.oldStart, h.newStart
	for _, group := range groups {
		result = append(result, hunk{oldStart: oldStart, newStart: newStart, section: h.section, lines: group})
		oldCount, newCount := lineCounts(group)
		oldStart += oldCount
		newStart += newCount
	}
	return result
}

type fileDiff struct {
	path    string
	header  string
	hunks   []hunk
	omitted bool
}

func (f *fileDiff) render() string {
	if f.omitted {
		return f.header + "(content omitted)\n"
	}
	var b strings.Builder
	b.WriteString(f.header)
	for i := range f.hunks {
		b.WriteString(f.hunks[i].render())
	}
	return b.String()
}

func (f *fileDiff) omit() fileDiff {
	result := *f
	if len(result.hunks) != 0 {
		result.hunks = nil
		result.omitted = true
	}
	return result
}

func (f *fileDiff) withContext(n int) fileDiff {
	result := *f
	result.hunks = make([]hunk, len(f.hunks))
	for i := range f.hunks {
		result.hunks[i] = f.hunks[i].withContext(n)
	}
	return result
}

func (f *fileDiff) split(limit int) []string {
	if text := f.render(); len(text) <= limit {
		return []string{text}
	}
	room := limit - len(f.header)
	var hunks []string
	for i := range f.hunks {
		for _, part := range f.hunks[i].split(room) {
			hunks = append(hunks, part.render())
		}
	}
	groups := pack(hunks, room)
	result := make([]string, len(groups))
	for i, group := range groups {
		result[i] = f.header + strings.Join(group, "")
	}
	return result
}

func lineCounts(lines []string) (oldCount, newCount int) {
	for _, line := range lines {
		oldCount += boolInt(strings.HasPrefix(line, " ") || strings.HasPrefix(line, "-"))
		newCount += boolInt(strings.HasPrefix(line, " ") || strings.HasPrefix(line, "+"))
	}
	return oldCount, newCount
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// parseDiff turns a unified patch into compact, stable LLM input. It accepts
// externally supplied patches in addition to md's normalized Git output.
// Path decoding and structural validation stay at this boundary. Low-value
// headers, lock/deletion bodies, and long lines are
// removed here before any request budget is calculated.
func parseDiff(text string) ([]fileDiff, error) {
	starts := regexp.MustCompile(`(?m)^diff --git `).FindAllStringIndex(text, -1)
	files := make([]fileDiff, 0, len(starts))
	for i, start := range starts {
		end := len(text)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		f, err := parseFileDiff(text[start[0]:end])
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

func parseFileDiff(block string) (fileDiff, error) {
	lines := strings.SplitAfter(block, "\n")
	f := fileDiff{path: extractPath(strings.TrimSuffix(lines[0], "\n")), header: clipDiffLine(lines[0])}
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			match := hunkPattern.FindStringSubmatch(strings.TrimSuffix(line, "\n"))
			if match == nil {
				return fileDiff{}, fmt.Errorf("malformed hunk header in %s: %s", f.path, strings.TrimSpace(line))
			}
			oldStart, _ := strconv.Atoi(match[1])
			newStart, _ := strconv.Atoi(match[2])
			f.hunks = append(f.hunks, hunk{oldStart: oldStart, newStart: newStart, section: match[3]})
			continue
		}
		switch {
		case len(f.hunks) != 0:
			f.hunks[len(f.hunks)-1].lines = append(f.hunks[len(f.hunks)-1].lines, clipDiffLine(line))
		case strings.HasPrefix(line, "+++ ") && strings.TrimSpace(line[4:]) != "/dev/null":
			var err error
			f.path, err = decodeGitPath(strings.TrimSpace(line[4:]))
			if err != nil {
				return fileDiff{}, err
			}
		case strings.HasPrefix(line, "rename to "):
			var err error
			f.path, err = decodeGitPath(strings.TrimSpace(line[len("rename to "):]))
			if err != nil {
				return fileDiff{}, err
			}
		case !strings.HasPrefix(line, "index ") && !strings.HasPrefix(line, "--- ") && !strings.HasPrefix(line, "+++ "):
			f.header += clipDiffLine(line)
		}
	}
	if isLockFile(f.path) || strings.Contains(f.header, "\ndeleted file mode ") {
		f = f.omit()
	}
	return f, nil
}

func clipDiffLine(line string) string {
	line = strings.TrimSuffix(line, "\n")
	if len(line) > maxDiffLine {
		end := maxDiffLine
		for end > 0 && !utf8.ValidString(line[:end]) {
			end--
		}
		line = line[:end] + " [truncated]"
	}
	return line + "\n"
}

func extractPath(line string) string {
	const prefix = "diff --git a/"
	if strings.HasPrefix(line, prefix) {
		rest := line[len(prefix):]
		if (len(rest)-3)%2 == 0 {
			pathLen := (len(rest) - 3) / 2
			if pathLen >= 0 && rest[pathLen:pathLen+3] == " b/" && rest[:pathLen] == rest[pathLen+3:] {
				return rest[:pathLen]
			}
		}
	}
	payload := strings.TrimPrefix(line, "diff --git ")
	if strings.HasPrefix(payload, `"`) {
		escaped := false
		for i := 1; i < len(payload); i++ {
			switch payload[i] {
			case '"':
				if !escaped {
					value, err := decodeGitPath(strings.TrimSpace(payload[i+1:]))
					if err == nil {
						return value
					}
					return ""
				}
				escaped = false
			case '\\':
				escaped = !escaped
			default:
				escaped = false
			}
		}
	}
	fields := strings.Fields(line)
	if len(fields) >= 4 {
		value, err := decodeGitPath(fields[len(fields)-1])
		if err == nil {
			return value
		}
	}
	return ""
}

func decodeGitPath(value string) (string, error) {
	if strings.HasPrefix(value, `"`) {
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("malformed quoted Git path %q: %w", value, err)
		}
		value = decoded
	}
	if strings.HasPrefix(value, "a/") || strings.HasPrefix(value, "b/") {
		value = value[2:]
	}
	return value, nil
}

func isTestFile(name string) bool {
	return strings.Contains(strings.ToLower(path.Base(name)), "test")
}

func isDataFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".json", ".jsonl", ".ndjson", ".yaml", ".yml":
		return true
	default:
		return false
	}
}

func isLockFile(name string) bool {
	base := strings.ToLower(path.Base(name))
	return strings.HasSuffix(base, ".lock") || base == "go.sum" || base == "npm-shrinkwrap.json" || base == "package-lock.json" || base == "pnpm-lock.yaml"
}

func reduceDiff(files []fileDiff, limit int, filters []func(string) bool, progress io.Writer) []fileDiff {
	if renderedLen(files) > limit {
		progressf(progress, "Diff is %d chars, over %d; reducing context to %d lines\n", renderedLen(files), limit, reducedContext)
		for i := range files {
			files[i] = files[i].withContext(reducedContext)
		}
	}
	for _, filter := range filters {
		if renderedLen(files) <= limit {
			break
		}
		progressf(progress, "Diff is %d chars, over %d; omitting low-value file bodies\n", renderedLen(files), limit)
		for i := range files {
			if filter(files[i].path) {
				files[i] = files[i].omit()
			}
		}
	}
	return files
}

func renderedLen(files []fileDiff) int {
	size := 0
	for i := range files {
		size += len(files[i].render())
	}
	return size
}

func pack(items []string, limit int) [][]string {
	if len(items) == 0 {
		return nil
	}
	total := 0
	for _, item := range items {
		total += len(item)
	}
	target := float64(total) / math.Max(1, math.Ceil(float64(total)/float64(limit)))
	var groups [][]string
	size := 0
	for _, item := range items {
		if len(groups) == 0 || size+len(item) > limit || float64(size)+float64(len(item))/2 > target {
			groups = append(groups, nil)
			size = 0
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], item)
		size += len(item)
	}
	return groups
}

func splitText(text string, limit int) []string {
	var pieces []string
	for len(text) > limit {
		end := limit
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		if boundary := max(strings.LastIndex(text[:end], "\n"), strings.LastIndex(text[:end], " ")); boundary > 0 {
			end = boundary + 1
		}
		pieces = append(pieces, text[:end])
		text = text[end:]
	}
	if text != "" {
		pieces = append(pieces, text)
	}
	return pieces
}

func splitDiff(files []fileDiff, limit int) []string {
	pieces := make([]string, 0, len(files))
	for i := range files {
		pieces = append(pieces, files[i].split(limit)...)
	}
	groups := pack(pieces, limit)
	result := make([]string, len(groups))
	for i, group := range groups {
		result[i] = strings.Join(group, "")
	}
	return result
}

func section(title, body string) string {
	return "=== " + title + " ===\n" + strings.TrimSpace(body) + "\n\n"
}

func room(budget int, overhead string) (int, error) {
	available := budget - len(overhead)
	if available < 4*maxDiffLine {
		return 0, fmt.Errorf("git metadata leaves %d of %d characters for changes; raise GIT_DESC_TOKENS", available, budget)
	}
	return available, nil
}

func contextBudget(tokens int) (int, error) {
	if tokens == 0 {
		if value := os.Getenv("GIT_DESC_TOKENS"); value != "" {
			var err error
			tokens, err = strconv.Atoi(value)
			if err != nil {
				return 0, fmt.Errorf("GIT_DESC_TOKENS must be an integer: %w", err)
			}
		} else {
			tokens = DefaultCommitMessageTokens
		}
	}
	if tokens < MinCommitMessageTokens {
		return 0, fmt.Errorf("commit message context must be at least %d tokens", MinCommitMessageTokens)
	}
	return int(float64(tokens) * (1 - reservedShare) * charsPerToken), nil
}

// GenerateCommitMsg generates a commit message from Git metadata and a unified diff.
//
// The input budget is derived from opts.ContextTokens or GIT_DESC_TOKENS and
// defaults to 64000.
// Large changes are reduced, split, summarized in parallel, and merged in rounds
// so every provider request remains within the budget. opts.Filters controls
// the progressive file-body omissions; nil uses the defaults.
func GenerateCommitMsg(ctx context.Context, p genai.Provider, metadata, diff string, opts *CommitMsgOptions) (string, error) {
	if opts == nil {
		opts = &CommitMsgOptions{}
	}
	filters := opts.Filters
	if filters == nil {
		filters = defaultDiffFilters
	}
	budget, err := contextBudget(opts.ContextTokens)
	if err != nil {
		return "", err
	}
	files, err := parseDiff(diff)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", errors.New("no changes to describe")
	}
	available, err := room(budget, metadata+section("Changes", ""))
	if err != nil {
		return "", err
	}
	files = reduceDiff(files, available, filters, opts.Progress)
	rendered := renderFiles(files)
	if len(rendered) <= available {
		return generate(ctx, p, commitMsgPrompt, metadata+section("Changes", rendered))
	}

	brief := opts.BriefMetadata
	if brief == "" {
		brief = briefMetadata(metadata)
	}
	available, err = room(budget, brief+section("Partial changes", ""))
	if err != nil {
		return "", err
	}
	parts := splitDiff(files, available)
	progressf(opts.Progress, "Diff is %d chars; splitting into %d parts\n", len(rendered), len(parts))
	inputs := make([]string, len(parts))
	for i, part := range parts {
		inputs[i] = brief + section("Partial changes", part)
	}
	summaries, err := generateAll(ctx, p, partPrompt, inputs)
	if err != nil {
		return "", err
	}
	available, err = room(budget, metadata+section("Part summaries", ""))
	if err != nil {
		return "", err
	}
	for summariesLen(summaries) > available {
		var pieces []string
		for _, item := range summaryItems(summaries) {
			pieces = append(pieces, splitText(item, available)...)
		}
		groups := pack(pieces, available)
		progressf(opts.Progress, "Summaries are %d chars, over %d; merging into %d parts\n", summariesLen(summaries), available, len(groups))
		mergeInputs := make([]string, len(groups))
		for i, group := range groups {
			mergeInputs[i] = strings.Join(group, "")
		}
		merged, mergeErr := generateAll(ctx, p, mergePrompt, mergeInputs)
		if mergeErr != nil {
			return "", mergeErr
		}
		if summariesLen(merged) >= summariesLen(summaries) {
			return "", errors.New("merging commit summaries did not shorten them; raise GIT_DESC_TOKENS")
		}
		summaries = merged
	}
	return generate(ctx, p, finalPrompt, metadata+section("Part summaries", strings.Join(summaryItems(summaries), "")))
}

func briefMetadata(metadata string) string {
	if before, _, ok := strings.Cut(metadata, "=== Recent Commits ==="); ok {
		return before
	}
	return metadata
}

func progressf(w io.Writer, format string, args ...any) {
	if w != nil {
		_, _ = fmt.Fprintf(w, format, args...)
	}
}

func renderFiles(files []fileDiff) string {
	var b strings.Builder
	for i := range files {
		b.WriteString(files[i].render())
	}
	return b.String()
}

func summaryItems(summaries []string) []string {
	items := make([]string, len(summaries))
	for i, summary := range summaries {
		items[i] = summary + "\n\n"
	}
	return items
}

func summariesLen(summaries []string) int {
	size := 0
	for _, summary := range summaries {
		size += len(summary) + 2
	}
	return size
}

func generateAll(ctx context.Context, p genai.Provider, prompt string, inputs []string) ([]string, error) {
	results := make([]string, len(inputs))
	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(maxParallelCalls)
	for i, input := range inputs {
		g.Go(func() error {
			result, err := generate(groupCtx, p, prompt, input)
			if err != nil {
				return err
			}
			results[i] = result
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

func generate(ctx context.Context, p genai.Provider, systemPrompt, content string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := p.GenSync(ctx, genai.Messages{genai.NewTextMessage(content)}, &genai.GenOptionText{SystemPrompt: systemPrompt})
	if err != nil {
		return "", err
	}
	answer := strings.TrimSpace(res.String())
	if answer == "" {
		return "", errors.New("commit message provider returned no output")
	}
	return answer, nil
}
