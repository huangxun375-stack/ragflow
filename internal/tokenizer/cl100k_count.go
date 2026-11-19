package tokenizer

import (
	"math"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/pkoukk/tiktoken-go"
)

const cl100kPattern = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`

func loadCL100KCountingEncoder() (*tiktoken.Tiktoken, *cl100kCountEncoder, error) {
	ranks, err := (localBpeLoader{}).LoadTiktokenBpe("https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken")
	if err != nil {
		return nil, nil, err
	}
	special := map[string]int{"<|endoftext|>": 100257, "<|fim_prefix|>": 100258, "<|fim_middle|>": 100259, "<|fim_suffix|>": 100260, "<|endofprompt|>": 100276}
	core, err := tiktoken.NewCoreBPE(ranks, special, cl100kPattern)
	if err != nil {
		return nil, nil, err
	}
	set := make(map[string]any, len(special))
	for s := range special {
		set[s] = true
	}
	enc := tiktoken.NewTiktoken(core, &tiktoken.Encoding{Name: "cl100k_base", PatStr: cl100kPattern, MergeableRanks: ranks, SpecialTokens: special}, set)
	return enc, &cl100kCountEncoder{ranks: ranks}, nil
}

// Each pre-token is an independent BPE merge domain; whole-input budgets and
// prefixes are deliberately not cached. Keys are copied so a tiny piece cannot
// retain an arbitrarily large document. The cap bounds both key bytes and entries.
type tokenCountMemo struct {
	mu     sync.RWMutex
	values map[string]int
}

func (m *tokenCountMemo) get(s string) (int, bool) {
	if len(s) > 128 {
		return 0, false
	}
	m.mu.RLock()
	n, ok := m.values[s]
	m.mu.RUnlock()
	return n, ok
}

func (m *tokenCountMemo) put(s string, n int) {
	if len(s) > 128 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.values[s]; ok {
		return
	}
	if len(m.values) >= 8192 {
		clear(m.values)
	}
	if m.values == nil {
		m.values = make(map[string]int)
	}
	m.values[strings.Clone(s)] = n
}

type cl100kCountEncoder struct {
	ranks map[string]int
	memo  tokenCountMemo
}

func (c *cl100kCountEncoder) count(s string) int {
	// tiktoken-go matches []rune(text), replacing malformed UTF-8 before BPE.
	if !utf8.ValidString(s) {
		s = string([]rune(s))
	}
	total := 0
	for start := 0; start < len(s); {
		end := cl100kPieceEnd(s, start)
		piece := s[start:end]
		if _, ok := c.ranks[piece]; ok {
			total++
		} else if n, ok := c.memo.get(piece); ok {
			total += n
		} else {
			n := cl100kMergeCount(piece, c.ranks)
			c.memo.put(piece, n)
			total += n
		}
		start = end
	}
	return total
}

func cl100kRune(s string, i int) (rune, int) {
	if s[i] < utf8.RuneSelf {
		return rune(s[i]), 1
	}
	return utf8.DecodeRuneInString(s[i:])
}

// Ordered alternatives of cl100kPattern, with byte offsets instead of regexp2
// Match objects. The whitespace lookahead leaves one rune before non-whitespace.
func cl100kPieceEnd(s string, i int) int {
	r, size := cl100kRune(s, i)
	if r == '\'' {
		for _, suffix := range []string{"s", "t", "re", "ve", "m", "ll", "d"} {
			at, ok := i+1, true
			for _, want := range suffix {
				if at == len(s) {
					ok = false
					break
				}
				x, n := cl100kRune(s, at)
				if unicode.ToLower(x) != want {
					ok = false
					break
				}
				at += n
			}
			if ok {
				return at
			}
		}
	}
	start := i
	if r != '\r' && r != '\n' && !unicode.IsLetter(r) && !unicode.IsNumber(r) {
		start += size
	}
	end := start
	for end < len(s) {
		x, n := cl100kRune(s, end)
		if !unicode.IsLetter(x) {
			break
		}
		end += n
	}
	if end > start {
		return end
	}
	if unicode.IsNumber(r) {
		end = i + size
		for n := 1; n < 3 && end < len(s); n++ {
			x, size := cl100kRune(s, end)
			if !unicode.IsNumber(x) {
				break
			}
			end += size
		}
		return end
	}
	start = i
	if r == ' ' {
		start++
	}
	end = start
	for end < len(s) {
		x, n := cl100kRune(s, end)
		if unicode.IsSpace(x) || unicode.IsLetter(x) || unicode.IsNumber(x) {
			break
		}
		end += n
	}
	if end > start {
		for end < len(s) && (s[end] == '\r' || s[end] == '\n') {
			end++
		}
		return end
	}
	end, lastNewline, lastStart := i, -1, i
	for end < len(s) {
		x, n := cl100kRune(s, end)
		if !unicode.IsSpace(x) {
			break
		}
		lastStart = end
		end += n
		if x == '\r' || x == '\n' {
			lastNewline = end
		}
	}
	if lastNewline >= 0 {
		return lastNewline
	}
	if end > i {
		if end < len(s) && lastStart > i {
			return lastStart
		}
		return end
	}
	return i + size
}

// Same rank order and leftmost tie break as tiktoken-go bytePairMerge; count
// the surviving boundaries rather than materializing the final token IDs.
func cl100kMergeCount(piece string, ranks map[string]int) int {
	var local [128][2]int
	parts := local[:0]
	if len(piece)+1 <= len(local) {
		parts = local[:len(piece)+1]
	} else {
		parts = make([][2]int, len(piece)+1)
	}
	for i := range parts {
		parts[i] = [2]int{i, math.MaxInt}
	}
	rank := func(i, skip int) int {
		if i+skip+2 < len(parts) {
			if r, ok := ranks[piece[parts[i][0]:parts[i+skip+2][0]]]; ok {
				return r
			}
		}
		return math.MaxInt
	}
	for i := 0; i < len(parts)-2; i++ {
		parts[i][1] = rank(i, 0)
	}
	for len(parts) > 1 {
		lowest, at := math.MaxInt, -1
		for i := 0; i < len(parts)-1; i++ {
			if parts[i][1] < lowest {
				lowest, at = parts[i][1], i
			}
		}
		if at < 0 {
			break
		}
		parts[at][1] = rank(at, 1)
		if at > 0 {
			parts[at-1][1] = rank(at-1, 1)
		}
		parts = append(parts[:at+1], parts[at+2:]...)
	}
	return len(parts) - 1
}
