package component

import (
	"strings"
	"unicode"
)

func codeSymbolKeywords(symbols []string) []string {
	keywords := make([]string, 0, len(symbols))
	seen := make(map[string]bool, len(symbols))
	for _, name := range symbols {
		name = sanitizeKeywordTerm(name)
		if name != "" && !seen[name] {
			seen[name] = true
			keywords = append(keywords, name)
		}
	}
	return keywords
}

// codeIdentifierTerms supplements the indexed text; it never changes displayed
// code or embedding input. Order is deterministic and repeated words collapse.
func codeIdentifierTerms(text string) string {
	words := []string{}
	seen := map[string]bool{}
	appendWord := func(word string) {
		word = strings.ToLower(word)
		if word != "" && !seen[word] {
			seen[word] = true
			words = append(words, word)
		}
	}
	for _, identifier := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		for _, part := range strings.Split(identifier, "_") {
			runes := []rune(part)
			start := 0
			for i := 1; i < len(runes); i++ {
				prev, current := runes[i-1], runes[i]
				boundary := (unicode.IsLower(prev) && unicode.IsUpper(current)) || (unicode.IsDigit(prev) != unicode.IsDigit(current))
				if i+1 < len(runes) && unicode.IsUpper(prev) && unicode.IsUpper(current) && unicode.IsLower(runes[i+1]) {
					boundary = true
				}
				if boundary {
					appendWord(string(runes[start:i]))
					start = i
				}
			}
			appendWord(string(runes[start:]))
		}
	}
	return strings.Join(words, " ")
}
