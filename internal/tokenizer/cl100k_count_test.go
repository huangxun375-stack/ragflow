package tokenizer

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
)

func TestCL100KScannerMatchesRegex(t *testing.T) {
	re := regexp2.MustCompile(cl100kPattern, regexp2.None)
	texts := perfSourceTexts(t)
	rng := rand.New(rand.NewSource(100237))
	for i := 0; i < 1000; i++ {
		var b strings.Builder
		for j := 0; j < 80; j++ {
			r := rune(rng.Intn(utf8.MaxRune + 1))
			if !utf8.ValidRune(r) {
				r = utf8.RuneError
			}
			b.WriteRune(r)
		}
		texts = append(texts, b.String())
	}
	texts = append(texts, "'S 'ſ 'K 'İ 'ı 'RE 've 'LL\r\n \t foo  123456 ١٢٣٤ \u0085\u2028x", string([]byte{0xff, 0xfe}))
	for i, text := range texts {
		s := string([]rune(text))
		m, err := re.FindStringMatch(s)
		if err != nil {
			t.Fatal(err)
		}
		at := 0
		for m != nil {
			end := cl100kPieceEnd(s, at)
			if s[at:end] != m.String() {
				t.Fatalf("case=%d offset=%d got=%q want=%q", i, at, s[at:end], m.String())
			}
			at = end
			m, err = re.FindNextMatch(m)
			if err != nil {
				t.Fatal(err)
			}
		}
		if at != len(s) {
			t.Fatalf("case=%d missing suffix", i)
		}
	}
	t.Logf("%d inputs: scanner pre-tokens identical to pinned regexp2", len(texts))
}

func TestTokenCountMemoBounded(t *testing.T) {
	var memo tokenCountMemo
	for i := 0; i < 20000; i++ {
		memo.put(string(rune(i))+"abc", i)
	}
	if len(memo.values) > 8192 {
		t.Fatal("unbounded memo")
	}
	memo.put(strings.Repeat("x", 129), 1)
	if _, ok := memo.get(strings.Repeat("x", 129)); ok {
		t.Fatal("oversized key admitted")
	}
}
