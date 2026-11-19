package nlp

import (
	"strings"
	"unicode"
)

func hasTestIntent(question string) bool {
	for _, word := range []string{"测试", "单测", "用例"} {
		if strings.Contains(question, word) {
			return true
		}
	}
	// Han text has no whitespace word boundaries; Latin words must not match
	// inside identifiers such as latest, contest, specification or mockery.
	for _, word := range strings.FieldsFunc(strings.ToLower(question), func(r rune) bool {
		return !(r == '_' || unicode.IsDigit(r) || unicode.IsLetter(r) && !unicode.Is(unicode.Han, r))
	}) {
		switch word {
		case "test", "tests", "testing", "unittest", "unittests", "spec", "specs", "mock", "mocks", "mocking", "benchmark", "benchmarks", "benchmarking":
			return true
		}
	}
	return false
}
