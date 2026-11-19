package codearchive

import "slices"

type sourceCheck struct {
	tokens           int
	exact            bool
	embeddingChecked bool
	embeddingFits    bool
}

func (b *chunkBuilder) symbolsFor(s byteSpan) []string {
	if b.spanSymbols == nil {
		b.spanSymbols = make(map[byteSpan][]string)
	}
	if names, ok := b.spanSymbols[s]; ok {
		return names
	}
	var names []string
	for i, decl := range b.symbols {
		if decl.kind == "import" || decl.start >= s.end || decl.end <= s.start {
			continue
		}
		if decl.start < s.start || decl.end > s.end {
			shadowed := false
			for _, child := range b.symbols[i+1:] {
				if child.start >= decl.end {
					break
				}
				if child.start < s.end && child.end > s.start && child.end <= decl.end && (child.start > decl.start || child.end < decl.end) {
					shadowed = true
					break
				}
			}
			if shadowed {
				continue
			}
		}
		if !slices.Contains(names, decl.name) {
			names = append(names, decl.name)
		}
	}
	b.spanSymbols[s] = names
	return names
}

func (b *chunkBuilder) sourceFits(s byteSpan, part int) bool {
	return b.sourceWithin(s, part, b.opts.ChunkTokens)
}

func (b *chunkBuilder) sourceWithin(s byteSpan, part, limit int) bool {
	// A builder owns one immutable source, symbol list and header policy. A
	// source span and part therefore identify the complete header plus body.
	key := fitKey{s, part}
	if b.checks == nil {
		b.checks = make(map[fitKey]sourceCheck)
	}
	check, exists := b.checks[key]
	text := ""
	if !exists || (!check.exact && check.tokens > limit) {
		text = b.sourceChunk(sourcePart{s, part}).Text
		check.tokens = len(text)
		if check.tokens > limit {
			check.tokens = b.opts.CountTokens(text)
			check.exact = true
		}
	}
	if check.tokens < 0 || check.tokens > limit {
		b.checks[key] = check
		return false
	}
	if !check.embeddingChecked {
		if text == "" {
			text = b.sourceChunk(sourcePart{s, part}).Text
		}
		check.embeddingFits = b.opts.FitsEmbedding(text)
		check.embeddingChecked = true
	}
	b.checks[key] = check
	return check.embeddingFits
}

func (b *chunkBuilder) target() int {
	return max(1, int(float64(b.opts.ChunkTokens)*b.opts.Policy.ChunkTargetRatio))
}

// Only adjacent complete structural units may merge. Oversized units recurse
// separately, so neither fragments nor a class header absorb neighboring units.
func (b *chunkBuilder) structure(s byteSpan, candidates []byteSpan, members bool) error {
	var units []byteSpan
	cursor := s.start
	for _, unit := range candidates {
		if unit.start < cursor || unit.end > s.end {
			continue
		}
		units = append(units, unit)
		cursor = unit.end
	}
	if len(units) == 0 {
		return b.split(s, nil)
	}
	pending := byteSpan{}
	flush := func() error {
		if pending.start == pending.end {
			return nil
		}
		err := b.split(pending, candidates)
		pending = byteSpan{}
		return err
	}
	target := b.target()
	cursor = s.start
	for i, unit := range units {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if cursor < unit.start {
			withPrefix := byteSpan{cursor, unit.end}
			if (!members || b.blank(byteSpan{cursor, unit.start})) && b.sourceFits(withPrefix, 0) {
				unit = withPrefix
			} else {
				if err := flush(); err != nil {
					return err
				}
				if err := b.split(byteSpan{cursor, unit.start}, nil); err != nil {
					return err
				}
			}
		}
		if i == len(units)-1 && unit.end < s.end {
			withSuffix := byteSpan{unit.start, s.end}
			if b.sourceFits(withSuffix, 0) {
				unit = withSuffix
			}
		}
		cursor = unit.end
		if b.opts.Policy.ChunkTargetRatio == 0 || !b.sourceWithin(unit, 0, target) {
			if err := flush(); err != nil {
				return err
			}
			if err := b.split(unit, candidates); err != nil {
				return err
			}
			continue
		}
		if pending.start != pending.end {
			merged := byteSpan{pending.start, unit.end}
			if pending.end == unit.start && b.sourceWithin(merged, 0, target) {
				pending = merged
				continue
			}
			if err := flush(); err != nil {
				return err
			}
		}
		pending = unit
	}
	if err := flush(); err != nil {
		return err
	}
	if cursor < s.end {
		return b.split(byteSpan{cursor, s.end}, nil)
	}
	return nil
}
