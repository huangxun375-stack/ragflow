package tokenizer

import (
	"math/rand"
	"strings"
	"testing"
)

const perfGoSource = `package ledger

import "fmt"

type Entry struct {
	Name string
	Amount int
}

// Summarize groups nonempty entries and returns their total.
func Summarize(entries []Entry) (map[string]int, int) {
	byName := make(map[string]int)
	total := 0
	for _, entry := range entries {
		if entry.Name == "" {
			continue
		}
		byName[entry.Name] += entry.Amount
		total += entry.Amount
	}
	return byName, total
}

// Label keeps Unicode names intact.
func (entry Entry) Label() string {
	return fmt.Sprintf("账目 %s: %d 已登记", entry.Name, entry.Amount)
}
`

func perfSourceTexts(t testing.TB) []string {
	t.Helper()
	texts := []string{"", "hello world ", "中文数据库与 English source", "😀🧑‍💻👩🏽‍🚀", "\x00\x01\x85\u200b\u2060\ufeff", "㍿㌀ﷺ", strings.Repeat("identifier_0123456789", 1024), "package p\n\n// 注释\nfunc F() { println(\"hello😀\") }\n"}
	rng := rand.New(rand.NewSource(237))
	alphabet := []rune(" abcXYZ_01239\n\t\r中英文😀🧑\u200b\ufeff\u2581\u0000\u0001\u0085\u2060㍿ﷺé\u0301")
	for i := 0; i < 200; i++ {
		runes := make([]rune, rng.Intn(160))
		for j := range runes {
			runes[j] = alphabet[rng.Intn(len(alphabet))]
		}
		texts = append(texts, strings.ReplaceAll(strings.ReplaceAll(string(runes), "\r\n", "\n"), "\r", "\n"))
	}
	return append(texts,
		perfGoSource,
		"// Keep the first value for each key.\nexport function unique(values: string[]): string[] {\n  return [...new Set(values)];\n}\n",
		"-- 合计非负数值。\nlocal function total(values)\n  local sum = 0\n  for _, value in ipairs(values) do\n    if value >= 0 then sum = sum + value end\n  end\n  return sum\nend\nreturn total\n",
	)
}

func BenchmarkTokenizerPerf(b *testing.B) {
	if err := InitCL100KEncoder(); err != nil {
		b.Fatal(err)
	}
	spm, err := LoadXLMRSentencePieceCounter()
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct{ name, text string }{
		{"source", perfGoSource},
		{"mixed", strings.Repeat("// 中文 English 😀\nfunc f() { return 123 }\n", 40)},
		{"longline", strings.Repeat("identifier_0123456789", 1024)},
	} {
		for _, counter := range []Counter{ResolveCounter(CounterCL100K), spm} {
			b.Run(counter.ID()+"/"+tc.name+"/Count", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(tc.text)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					perfCountSink = counter.Count(tc.text)
				}
			})
			b.Run(counter.ID()+"/"+tc.name+"/Trim", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					perfTextSink = counter.TrimToLimit(tc.text, 512)
				}
			})
		}
	}
}

var perfCountSink int
var perfTextSink string
