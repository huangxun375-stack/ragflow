package tokenizer

import (
	"strings"
	"testing"
)

func TestPerfLimiterFitsTrimEquivalence(t *testing.T) {
	texts := append(perfSourceTexts(t), strings.Repeat("12345 中文 ", 4000), string([]byte{0xff, 0xfe}))
	for _, id := range []string{CounterCL100K, CounterXLMRSentence, CounterBERTWordPiece, CounterQwenBPE, CounterLlamaBPE, "missing"} {
		counter, available := CounterByID(id)
		if !available && id != "missing" {
			t.Fatalf("%s asset unavailable", id)
		}
		cal := NewCalibration(1.3)
		for _, limiter := range []Limiter{NewExactLimiter(counter), NewCalibratedLimiter(counter, "perf", cal)} {
			for _, maxTokens := range []int{-1, 1, 32, 512, 8192} {
				for i, s := range texts {
					trimmed, _ := limiter.Trim(s, maxTokens)
					if got := limiter.Fits(s, maxTokens); got != (trimmed == s) {
						t.Fatalf("%s case=%d max=%d: Fits differs from Trim", id, i, maxTokens)
					}
				}
			}
		}
	}
}

func BenchmarkLimiterFitsPerf(b *testing.B) {
	c, err := LoadXLMRSentencePieceCounter()
	if err != nil {
		b.Fatal(err)
	}
	l := NewExactLimiter(c)
	for _, n := range []int{40, 4000} {
		s := strings.Repeat("// 中文 English\nfunc f() { return 123 }\n", n)
		name := "fitting"
		if n > 40 {
			name = "overlimit"
		}
		b.Run(name+"/TrimEquals", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				trimmed, _ := l.Trim(s, 8192)
				perfFitsSink = trimmed == s
			}
		})
		b.Run(name+"/Fits", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				perfFitsSink = l.Fits(s, 8192)
			}
		})
	}
}

var perfFitsSink bool
