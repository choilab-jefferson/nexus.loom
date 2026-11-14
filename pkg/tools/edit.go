package tools

// edit_file after earendil-works/pi (packages/coding-agent/src/core/tools/edit.ts, edit-diff.ts):
//
//   - one call carries one or more blocks {target, replacement}; every block is matched against the file as it is
//     now (read under the file's write lock, right before the write), not against earlier blocks' results;
//   - a target must occur exactly once; 0 or >1 occurrences, overlapping blocks or a result identical to the file
//     are errors, and any error changes nothing (all or nothing);
//   - matching is exact first (line endings and a BOM ignored); when that finds nothing, a tolerant match ignores
//     trailing whitespace and maps typographic quotes, dashes and special spaces to ASCII;
//   - only the matched bytes are replaced: a BOM, CRLF line endings and every untouched byte stay as they are, and
//     newlines in a replacement take the file's line ending;
//   - the result reports the blocks and a unified diff of the change.

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

type editBlock struct{ target, replacement string }

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

// parseEdits reads `edits` (an array of {target, replacement}, a JSON string of it, or one object) and the legacy
// top-level target/replacement. pi's oldText/newText (and old_text/new_text) are accepted as aliases.
func parseEdits(args map[string]any) ([]editBlock, error) {
	var raw []any
	switch v := args["edits"].(type) {
	case nil:
	case []any:
		raw = v
	case map[string]any:
		raw = []any{v}
	case string:
		if strings.TrimSpace(v) != "" {
			if err := json.Unmarshal([]byte(v), &raw); err != nil {
				return nil, fmt.Errorf("edit_file: edits must be an array of {target, replacement}")
			}
		}
	default:
		return nil, fmt.Errorf("edit_file: edits must be an array of {target, replacement}")
	}
	var out []editBlock
	for i, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("edit_file: edits[%d] must be an object {target, replacement}", i)
		}
		t, _ := firstString(m, "target", "old_text", "oldText")
		rep, ok := firstString(m, "replacement", "new_text", "newText")
		if !ok {
			return nil, fmt.Errorf("edit_file: edits[%d] needs a replacement", i)
		}
		out = append(out, editBlock{t, rep})
	}
	if t, ok := firstString(args, "target"); ok {
		rep, _ := firstString(args, "replacement")
		out = append(out, editBlock{t, rep})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("edit_file: give at least one block in edits: [{target, replacement}]")
	}
	return out, nil
}

// view is a normalised copy of the file for matching. start[k]/end[k] are the original byte range that produced
// view byte k, so a match in the view maps back to exactly the original bytes it covers.
type view struct {
	text       string
	start, end []int
	tolerant   bool
}

func isSpecialSpace(r rune) bool {
	return r == ' ' || (r >= ' ' && r <= ' ') || r == ' ' || r == ' ' || r == '　'
}

func tolerantRune(r rune) rune {
	switch r {
	case '‘', '’', '‚', '‛':
		return '\''
	case '“', '”', '„', '‟':
		return '"'
	case '‐', '‑', '‒', '–', '—', '―', '−':
		return '-'
	}
	if isSpecialSpace(r) {
		return ' '
	}
	return r
}

func isTrailingSpace(r rune) bool { return r == ' ' || r == '\t' || isSpecialSpace(r) }

// buildView normalises content[from:]: CRLF -> LF always; tolerant also trims trailing whitespace per line and maps
// typography to ASCII.
func buildView(content string, from int, tolerant bool) view {
	var b strings.Builder
	v := view{tolerant: tolerant}
	emit := func(s string, os, oe int) {
		b.WriteString(s)
		for i := 0; i < len(s); i++ {
			v.start = append(v.start, os)
			v.end = append(v.end, oe)
		}
	}
	pos := from
	for pos <= len(content) {
		nl := strings.IndexByte(content[pos:], '\n')
		lineEnd, next := len(content), len(content)+1
		if nl >= 0 {
			lineEnd, next = pos+nl, pos+nl+1
		}
		textEnd := lineEnd
		if textEnd > pos && content[textEnd-1] == '\r' && nl >= 0 {
			textEnd--
		}
		rawEnd := textEnd // the line's text before CR/LF, trailing whitespace included
		if tolerant {
			for textEnd > pos {
				r, size := utf8.DecodeLastRuneInString(content[pos:textEnd])
				if !isTrailingSpace(r) {
					break
				}
				textEnd -= size
			}
		}
		for p := pos; p < textEnd; {
			r, size := utf8.DecodeRuneInString(content[p:])
			s := content[p : p+size]
			if tolerant {
				if t := tolerantRune(r); t != r {
					s = string(t)
				}
			}
			emit(s, p, p+size)
			p += size
		}
		if nl >= 0 {
			// the view's newline stands for the line's end: trimmed whitespace, CR and LF
			emit("\n", rawEnd, next)
		}
		pos = next
	}
	v.text = b.String()
	return v
}

func normaliseTarget(t string, tolerant bool) string {
	t = strings.ReplaceAll(t, "\r\n", "\n")
	if !tolerant {
		return t
	}
	lines := strings.Split(t, "\n")
	for i, l := range lines {
		l = strings.TrimRightFunc(l, isTrailingSpace)
		lines[i] = strings.Map(tolerantRune, l)
	}
	return strings.Join(lines, "\n")
}

type span struct{ start, end, block int }

// locate finds the one place a target covers in the original content.
func locate(content string, views [2]view, b editBlock, i, n int, rel string) (span, error) {
	name := "the target"
	if n > 1 {
		name = fmt.Sprintf("edits[%d]", i)
	}
	if b.target == "" {
		return span{}, fmt.Errorf("%s in %s is empty", name, rel)
	}
	for _, v := range views {
		t := normaliseTarget(b.target, v.tolerant)
		if t == "" {
			continue
		}
		switch c := strings.Count(v.text, t); {
		case c > 1:
			return span{}, fmt.Errorf("found %d occurrences of %s in %s; a target must be unique: add more context around it", c, name, rel)
		case c == 1:
			k := strings.Index(v.text, t)
			s, e := v.start[k], v.end[k+len(t)-1]
			if v.tolerant && (k+len(t) == len(v.text) || v.text[k+len(t)] == '\n') {
				// a match to the end of a line also takes that line's trimmed trailing whitespace
				if k+len(t) == len(v.text) {
					e = len(content)
				} else {
					e = v.start[k+len(t)]
				}
			}
			return span{s, e, i}, nil
		}
	}
	return span{}, fmt.Errorf("%s not found in %s: it must match the file exactly, including whitespace and line breaks (read the file again)", name, rel)
}

// applyEdits returns the new content, or an error that leaves the file untouched.
func applyEdits(content string, blocks []editBlock, rel string) (string, error) {
	from := 0
	if strings.HasPrefix(content, "\ufeff") {
		from = len("\ufeff")
	}
	views := [2]view{buildView(content, from, false), buildView(content, from, true)}
	spans := make([]span, 0, len(blocks))
	for i, b := range blocks {
		s, err := locate(content, views, b, i, len(blocks), rel)
		if err != nil {
			return "", err
		}
		spans = append(spans, s)
	}
	sort.Slice(spans, func(a, b int) bool { return spans[a].start < spans[b].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return "", fmt.Errorf("edits[%d] and edits[%d] overlap in %s: merge them into one block or target separate places",
				spans[i-1].block, spans[i].block, rel)
		}
	}
	crlf := strings.Count(content, "\r\n")*2 > strings.Count(content, "\n")
	out := content
	for i := len(spans) - 1; i >= 0; i-- {
		rep := strings.ReplaceAll(blocks[spans[i].block].replacement, "\r\n", "\n")
		if crlf {
			rep = strings.ReplaceAll(rep, "\n", "\r\n")
		}
		out = out[:spans[i].start] + rep + out[spans[i].end:]
	}
	if out == content {
		return "", fmt.Errorf("no change in %s: the replacement(s) produce the same content", rel)
	}
	return out, nil
}

func execEditFile(args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	relPath := getString(args, "path")
	blocks, err := parseEdits(args)
	if err != nil {
		return "", err
	}
	resolved, err := policy.ResolveScopedPath(cfg.Workspace, relPath, true, cfg)
	if err != nil {
		return "", err
	}
	f, err := openLocked(resolved, false) // read, match and write under the file's lock: the file as it is now
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	rel := filepath.ToSlash(relPath)
	updated, err := applyEdits(string(data), blocks, rel)
	if err != nil {
		return "", err
	}
	if err := rewriteLocked(f, []byte(updated)); err != nil {
		return "", err
	}
	cfg.Seen.Record(resolved, []byte(updated))
	return fmt.Sprintf("edited %s: replaced %d block(s)\n%s", rel, len(blocks), unifiedDiff(string(data), updated, rel)), nil
}

// ---- unified diff of the change (what the model gets back) ----

func diffLines(s string) []string {
	s = strings.TrimPrefix(strings.ReplaceAll(s, "\r\n", "\n"), "\ufeff")
	l := strings.Split(s, "\n")
	if len(l) > 0 && l[len(l)-1] == "" {
		l = l[:len(l)-1]
	}
	return l
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	line string
}

// lineOps diffs two line lists: common prefix and suffix, then an LCS of the middle (a plain replace when the middle
// is too large to compare cheaply).
func lineOps(a, b []string) []diffOp {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	var ops []diffOp
	for _, l := range a[:p] {
		ops = append(ops, diffOp{' ', l})
	}
	ma, mb := a[p:len(a)-s], b[p:len(b)-s]
	if len(ma)*len(mb) > 4_000_000 {
		for _, l := range ma {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range mb {
			ops = append(ops, diffOp{'+', l})
		}
	} else {
		dp := make([][]int, len(ma)+1)
		for i := range dp {
			dp[i] = make([]int, len(mb)+1)
		}
		for i := len(ma) - 1; i >= 0; i-- {
			for j := len(mb) - 1; j >= 0; j-- {
				if ma[i] == mb[j] {
					dp[i][j] = dp[i+1][j+1] + 1
				} else if dp[i+1][j] >= dp[i][j+1] {
					dp[i][j] = dp[i+1][j]
				} else {
					dp[i][j] = dp[i][j+1]
				}
			}
		}
		i, j := 0, 0
		for i < len(ma) || j < len(mb) {
			switch {
			case i < len(ma) && j < len(mb) && ma[i] == mb[j]:
				ops = append(ops, diffOp{' ', ma[i]})
				i, j = i+1, j+1
			case j < len(mb) && (i == len(ma) || dp[i][j+1] >= dp[i+1][j]):
				ops = append(ops, diffOp{'+', mb[j]})
				j++
			default:
				ops = append(ops, diffOp{'-', ma[i]})
				i++
			}
		}
	}
	for _, l := range a[len(a)-s:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

const diffContext = 3

func unifiedDiff(oldS, newS, rel string) string {
	ops := lineOps(diffLines(oldS), diffLines(newS))
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", rel, rel)
	n := len(ops)
	for i := 0; i < n; {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		// a hunk: from diffContext lines before this change to diffContext after the last change near it
		start := i - diffContext
		if start < 0 {
			start = 0
		}
		end := i
		for end < n {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < n && ops[run].kind == ' ' {
				run++
			}
			if run < n && run-end <= 2*diffContext {
				end = run
				continue
			}
			end += diffContext
			if end > n {
				end = n
			}
			break
		}
		oldLine, newLine := 1, 1
		for _, op := range ops[:start] {
			if op.kind != '+' {
				oldLine++
			}
			if op.kind != '-' {
				newLine++
			}
		}
		oldN, newN := 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				oldN++
			}
			if op.kind != '-' {
				newN++
			}
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oldLine, oldN, newLine, newN)
		for _, op := range ops[start:end] {
			b.WriteByte(op.kind)
			b.WriteString(op.line)
			b.WriteByte('\n')
		}
		i = end
	}
	return b.String()
}
