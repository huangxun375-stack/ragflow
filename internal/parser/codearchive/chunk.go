package codearchive

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type sourcePart struct {
	byteSpan
	part int
}

type chunkBuilder struct {
	ctx          context.Context
	opts         Options
	file         SourceFile
	lines        []int
	result       *Result
	source       []sourcePart
	stagedChunks int
	stagedBlank  bool
	symbols      []symbol
	syntax       *syntax
	checks       map[fitKey]sourceCheck
	spanSymbols  map[byteSpan][]string
}

type fitKey struct {
	span byteSpan
	part int
}

func (b *chunkBuilder) sourceLines(s byteSpan) (int, int) {
	start := sort.Search(len(b.lines), func(i int) bool { return b.lines[i] > s.start })
	end := sort.Search(len(b.lines), func(i int) bool { return b.lines[i] > max(s.start, s.end-1) })
	if b.file.LineMap != nil {
		mappedStart, mappedEnd := b.file.LineMap[start-1].Start, b.file.LineMap[start-1].End
		for _, line := range b.file.LineMap[start-1 : end] {
			mappedStart = min(mappedStart, line.Start)
			mappedEnd = max(mappedEnd, line.End)
		}
		return mappedStart, mappedEnd
	}
	return start, end
}

func (b *chunkBuilder) chunk(s byteSpan, body string, part int) Chunk {
	return b.namedChunk(s, body, part, b.symbolsFor(s))
}

func (b *chunkBuilder) namedChunk(s byteSpan, body string, part int, names []string) Chunk {
	start, end := b.sourceLines(s)
	header := fmt.Sprintf("// %s:L%d-L%d", b.file.Path, start, end)
	if part != 0 {
		header += fmt.Sprintf("#%d", part)
	}
	if len(names) > 0 {
		header += " " + truncateUTF8(strings.Join(names, ", "), min(160, max(8, b.opts.ChunkTokens/3)))
	}
	return Chunk{Text: header + "\n" + body, Symbols: names, SourcePath: b.file.Path, Language: b.file.Language, StartLine: start, EndLine: end, Order: len(b.result.Chunks)}
}

func (b *chunkBuilder) fits(text string) bool {
	// cl100k is byte BPE: it cannot emit more tokens than input bytes.
	// The independent embedding predicate has no such contract.
	if len(text) > b.opts.ChunkTokens {
		tokens := b.opts.CountTokens(text)
		if tokens < 0 || tokens > b.opts.ChunkTokens {
			return false
		}
	}
	return b.opts.FitsEmbedding(text)
}

func (b *chunkBuilder) append(c Chunk) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if !b.fits(c.Text) {
		return fail("ZIP_CONFIG_INVALID")
	}
	if len(b.result.Chunks) >= b.opts.Policy.Limits.MaxChunks {
		return fail("ZIP_TOO_MANY_CHUNKS")
	}
	c.Order = len(b.result.Chunks)
	b.result.Chunks = append(b.result.Chunks, c)
	return nil
}

func (b *chunkBuilder) split(s byteSpan, candidates []byteSpan) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if s.start == s.end {
		return nil
	}
	if b.blank(s) {
		return b.stage(s, 0)
	}
	if b.sourceFits(s, 0) {
		return b.stage(s, 0)
	}
	if !b.fits(b.chunk(s, "", 0).Text) {
		return fail("ZIP_CONFIG_INVALID")
	}
	var descendants []byteSpan
	for _, unit := range candidates {
		if unit != s && unit.start >= s.start && unit.end <= s.end {
			descendants = append(descendants, unit)
		}
	}
	if len(descendants) > 0 {
		return b.structure(s, descendants, true)
	}
	return b.splitLines(s)
}

func (b *chunkBuilder) mergeLines(parts []byteSpan) error {
	for at, part := 0, 1; at < len(parts); part++ {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		best, upper := at, len(parts)
		for step := 1; ; step *= 2 {
			end := min(len(parts), at+step)
			if !b.sourceFits(byteSpan{parts[at].start, parts[end-1].end}, part) {
				upper = end - 1
				break
			}
			best = end
			if end == len(parts) {
				break
			}
		}
		for lo, hi := best+1, upper; lo <= hi; {
			mid := lo + (hi-lo)/2
			if b.sourceFits(byteSpan{parts[at].start, parts[mid-1].end}, part) {
				best, lo = mid, mid+1
			} else {
				hi = mid - 1
			}
		}
		if best > at && best < len(parts) {
			var err error
			best, err = b.syntaxCut(parts, at, best, part)
			if err != nil {
				return err
			}
		}
		if best == at {
			if err := b.split(parts[at], nil); err != nil {
				return err
			}
			at++
		} else {
			if err := b.stage(byteSpan{parts[at].start, parts[best-1].end}, part); err != nil {
				return err
			}
			at = best
		}
	}
	return nil
}

func (b *chunkBuilder) syntaxCut(parts []byteSpan, at, best, part int) (int, error) {
	if b.syntax == nil || b.syntax.tree == nil {
		return best, nil
	}
	chosen, shallowest := best, int(^uint(0)>>1)
	for j := best; j >= at+(best-at+1)/2; j-- {
		if err := b.ctx.Err(); err != nil {
			return 0, err
		}
		line := sort.SearchInts(b.lines, parts[j].start)
		if line == len(b.lines) || b.lines[line] != parts[j].start {
			continue
		}
		depth, ok := b.syntax.cutDepth(b.lines, line)
		if !ok || depth >= shallowest {
			continue
		}
		// BPE counts and embedding predicates need not be prefix-monotonic.
		// Recheck the complete shortened chunk, including its changed header.
		if b.sourceFits(byteSpan{parts[at].start, parts[j-1].end}, part) {
			chosen, shallowest = j, depth
		}
	}
	return chosen, nil
}

func (b *chunkBuilder) splitLines(s byteSpan) error {
	var lines []byteSpan
	start := s.start
	for i := s.start; i < s.end; i++ {
		if b.file.Text[i] == '\n' {
			lines = append(lines, byteSpan{start, i + 1})
			start = i + 1
		}
	}
	if start < s.end {
		lines = append(lines, byteSpan{start, s.end})
	}
	if len(lines) > 1 {
		return b.mergeLines(lines)
	}
	return b.splitRunes(s)
}

func (b *chunkBuilder) splitRunes(s byteSpan) error {
	start, part := s.start, 1
	for start < s.end {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if b.blank(byteSpan{start, s.end}) {
			return b.stage(byteSpan{start, s.end}, part)
		}
		// Exponential probing bounds work by the size of a fitting prefix,
		// instead of recounting a megabyte-long remainder for each tiny chunk.
		best := start
		step := 1
		upper := s.end
		for probe := start; probe < s.end; {
			next := min(s.end, start+step)
			for next < s.end && !utf8.RuneStart(b.file.Text[next]) {
				next++
			}
			candidate := byteSpan{start, next}
			if !b.sourceFits(candidate, part) {
				upper = next
				break
			}
			best, probe = next, next
			if next == s.end {
				break
			}
			step *= 2
		}
		lo, hi := best, upper
		for lo < hi {
			mid := lo + (hi-lo+1)/2
			for mid < hi && !utf8.RuneStart(b.file.Text[mid]) {
				mid++
			}
			if mid <= best {
				break
			}
			candidate := byteSpan{start, mid}
			if b.sourceFits(candidate, part) {
				best, lo = mid, mid
			} else {
				hi = mid - 1
				for hi > best && !utf8.RuneStart(b.file.Text[hi]) {
					hi--
				}
			}
		}
		if best == start {
			return fail("ZIP_CONFIG_INVALID")
		}
		if err := b.stage(byteSpan{start, best}, part); err != nil {
			return err
		}
		start, part = best, part+1
	}
	return nil
}

func (b *chunkBuilder) sourceChunk(p sourcePart) Chunk {
	return b.chunk(p.byteSpan, b.file.Text[p.start:p.end], p.part)
}

func (b *chunkBuilder) blank(s byteSpan) bool {
	return strings.TrimSpace(b.file.Text[s.start:s.end]) == ""
}

func (b *chunkBuilder) stage(s byteSpan, part int) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	blank := b.blank(s)
	if !blank {
		if len(b.result.Chunks)+b.stagedChunks >= b.opts.Policy.Limits.MaxChunks {
			return fail("ZIP_TOO_MANY_CHUNKS")
		}
		b.stagedChunks++
	}
	// Coalesced whitespace is pending source, not an indexable chunk. It need
	// not fit until assigned to nonblank neighbors by finishSource.
	if n := len(b.source); n > 0 && blank && b.stagedBlank {
		b.source[n-1].end = s.end
	} else {
		b.source = append(b.source, sourcePart{s, part})
	}
	b.stagedBlank = blank
	return nil
}

// finishSource only changes boundaries: every byte still occurs exactly once.
// Fitting syntax units are protected even if whitespace needs redistribution.
// Some inputs cannot meet all three constraints (lossless, nonblank, bounded);
// fail atomically rather than dropping whitespace or exceeding either budget.
func (b *chunkBuilder) finishSource(candidates []byteSpan) error {
	written := 0
	for i := 0; i < len(b.source); i++ {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		gap := b.source[i]
		if !b.blank(gap.byteSpan) {
			b.source[written] = gap
			written++
			continue
		}
		if written > 0 {
			left := b.source[written-1]
			left.end = gap.end
			if b.sourceFits(left.byteSpan, left.part) {
				b.source[written-1] = left
				continue
			}
		}
		if i+1 < len(b.source) {
			right := b.source[i+1]
			right.start = gap.start
			if b.sourceFits(right.byteSpan, right.part) {
				b.source[i+1] = right
				continue
			}
		}
		if written > 0 && i+1 < len(b.source) {
			left, right := b.source[written-1], b.source[i+1]
			if a, z, ok := b.rebalance(left, right, gap.start, gap.end, nil); ok {
				b.source[written-1], b.source[i+1] = a, z
				continue
			}
		}
		// If a long-line fragment filled its budget, make room for adjacent
		// whitespace by splitting that fragment, never a fitting declaration.
		repaired := false
		for _, neighbor := range []int{written - 1, i + 1} {
			if neighbor < 0 || neighbor >= len(b.source) {
				continue
			}
			p := b.source[neighbor]
			left, right := p, p
			left.start, right.end = min(p.start, gap.start), max(p.end, gap.end)
			if a, z, ok := b.rebalance(left, right, p.start, p.end, candidates); ok {
				if len(b.result.Chunks)+b.stagedChunks >= b.opts.Policy.Limits.MaxChunks {
					return fail("ZIP_TOO_MANY_CHUNKS")
				}
				b.stagedChunks++
				if neighbor == written-1 {
					b.source[neighbor], b.source[written] = a, z
				} else {
					b.source[written], b.source[neighbor] = a, z
				}
				written++
				repaired = true
				break
			}
		}
		if !repaired {
			if err := b.ctx.Err(); err != nil {
				return err
			}
			return fail("ZIP_CONFIG_INVALID")
		}
	}
	b.source = b.source[:written]
	for _, p := range b.source {
		if b.blank(p.byteSpan) {
			return fail("ZIP_CONFIG_INVALID")
		}
		if !b.sourceFits(p.byteSpan, p.part) {
			return fail("ZIP_CONFIG_INVALID")
		}
		if len(b.result.Chunks) >= b.opts.Policy.Limits.MaxChunks {
			return fail("ZIP_TOO_MANY_CHUNKS")
		}
		c := b.sourceChunk(p)
		c.Order = len(b.result.Chunks)
		b.result.Chunks = append(b.result.Chunks, c)
	}
	return nil
}

// rebalance searches a bounded rune-aligned boundary interval. Binary probing
// is a conservative search (BPE counts need not be monotonic); both final texts
// are always checked exactly. A missed feasible split fails, never overflows.
func (b *chunkBuilder) rebalance(left, right sourcePart, lo, hi int, candidates []byteSpan) (sourcePart, sourcePart, bool) {
	var protected []byteSpan
	for _, unit := range candidates {
		if b.ctx.Err() != nil {
			return left, right, false
		}
		if unit.start < hi && unit.end > lo && b.sourceFits(unit, 0) {
			protected = append(protected, unit)
		}
	}
	try := func(at int) bool {
		if at <= left.start || at >= right.end {
			return false
		}
		for _, unit := range protected {
			if unit.start < at && at < unit.end {
				return false
			}
		}
		left.end, right.start = at, at
		return !b.blank(left.byteSpan) && !b.blank(right.byteSpan) && b.sourceFits(left.byteSpan, left.part) && b.sourceFits(right.byteSpan, right.part)
	}
	// Small gaps are cheap to exhaust, and structural edges may be the only
	// legal positions even when binary probing would jump past them.
	if hi-lo <= 128 {
		for at := lo; at <= hi; {
			if b.ctx.Err() != nil {
				return left, right, false
			}
			if try(at) {
				return left, right, true
			}
			_, size := utf8.DecodeRuneInString(b.file.Text[at:])
			if size == 0 {
				break
			}
			at += size
		}
		return left, right, false
	}
	for _, unit := range protected {
		for _, at := range []int{unit.start, unit.end} {
			if b.ctx.Err() != nil {
				return left, right, false
			}
			if at >= lo && at <= hi && try(at) {
				return left, right, true
			}
		}
	}
	for lo <= hi {
		if b.ctx.Err() != nil {
			return left, right, false
		}
		mid := lo + (hi-lo)/2
		for mid > lo && mid < len(b.file.Text) && !utf8.RuneStart(b.file.Text[mid]) {
			mid--
		}
		if mid <= left.start {
			_, size := utf8.DecodeRuneInString(b.file.Text[mid:])
			lo = mid + size
			continue
		}
		if mid >= right.end {
			_, size := utf8.DecodeLastRuneInString(b.file.Text[:mid])
			hi = mid - size
			continue
		}
		left.end, right.start = mid, mid
		allowed, start, end := true, mid, mid
		for _, unit := range protected {
			if unit.start < mid && mid < unit.end {
				allowed = false
				start, end = min(start, unit.start), max(end, unit.end)
			}
		}
		leftFits := b.sourceFits(left.byteSpan, left.part)
		if allowed && leftFits && !b.blank(left.byteSpan) && !b.blank(right.byteSpan) && b.sourceFits(right.byteSpan, right.part) {
			return left, right, true
		}
		if leftFits && !b.blank(right.byteSpan) {
			_, size := utf8.DecodeRuneInString(b.file.Text[mid:])
			lo = max(mid+size, end)
		} else {
			_, size := utf8.DecodeLastRuneInString(b.file.Text[:mid])
			hi = min(mid-size, start)
		}
	}
	return left, right, false
}

func (b *chunkBuilder) overview(symbols []symbol) error {
	if len(symbols) == 0 {
		return nil
	}
	part := 1
	prefix := func() string { return fmt.Sprintf("概览 %s [%s] #%d\n", b.file.Path, b.file.Language, part) }
	body := prefix()
	span := byteSpan{symbols[0].start, symbols[0].end}
	for _, s := range symbols {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		line := s.name + ": " + s.signature
		if s.kind == "import" {
			line = "imports: " + compactLine(s.name, 240)
		}
		if s.doc != "" {
			line += " — " + s.doc
		}
		line = compactLine(line, 400) + "\n"
		nextSpan := byteSpan{min(span.start, s.start), max(span.end, s.end)}
		if !b.fits(b.namedChunk(nextSpan, body+line, 0, nil).Text) && body != prefix() {
			if err := b.append(b.namedChunk(span, body, 0, nil)); err != nil {
				return err
			}
			part++
			body = prefix()
			nextSpan = byteSpan{s.start, s.end}
		}
		// An overview is a bounded summary, not source: shorten a single long
		// signature rather than rejecting a losslessly representable file.
		for !b.fits(b.namedChunk(nextSpan, body+line, 0, nil).Text) {
			if len(line) <= len(" …\n") {
				return fail("ZIP_CONFIG_INVALID")
			}
			line = truncateUTF8(strings.TrimSuffix(line, "\n"), max(len(" …"), len(line)*3/4)) + "\n"
		}
		body += line
		span = nextSpan
	}
	return b.append(b.namedChunk(span, body, 0, nil))
}
