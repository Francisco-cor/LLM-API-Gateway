package pricing

// Model patterns are globs. Unlike path.Match, '*' crosses '/' so a pattern
// such as "meta-llama/*" matches "meta-llama/Llama-3-70B" (model identifiers
// frequently contain provider namespaces). '?', character classes and '\'
// escapes are supported for compatibility with path.Match patterns.
type globKind int

const (
	globLiteral globKind = iota
	globAny              // ?
	globStar             // *
	globClass            // [...]
)

type globToken struct {
	kind  globKind
	r     rune
	class globClassSpec
}

type globClassSpec struct {
	negated bool
	ranges  []rune // flattened lo, hi pairs
}

type ruleScore struct {
	exact    bool
	literals int // literal runes plus one per character class
	prefix   int // leading literal runes before the first wildcard
}

func parseGlob(pattern string) ([]globToken, bool) {
	runes := []rune(pattern)
	tokens := make([]globToken, 0, len(runes))
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '*':
			if len(tokens) > 0 && tokens[len(tokens)-1].kind == globStar {
				continue
			}
			tokens = append(tokens, globToken{kind: globStar})
		case '?':
			tokens = append(tokens, globToken{kind: globAny})
		case '\\':
			if i+1 >= len(runes) {
				return nil, false
			}
			i++
			tokens = append(tokens, globToken{kind: globLiteral, r: runes[i]})
		case '[':
			class, next, ok := parseClass(runes, i)
			if !ok {
				return nil, false
			}
			tokens = append(tokens, globToken{kind: globClass, class: class})
			i = next
		default:
			tokens = append(tokens, globToken{kind: globLiteral, r: runes[i]})
		}
	}
	return tokens, true
}

// parseClass parses a character class starting at the '[' at index start and
// returns the index of the closing ']'.
func parseClass(runes []rune, start int) (globClassSpec, int, bool) {
	var class globClassSpec
	i := start + 1
	if i < len(runes) && (runes[i] == '!' || runes[i] == '^') {
		class.negated = true
		i++
	}
	first := true
	for i < len(runes) {
		if runes[i] == ']' && !first {
			if len(class.ranges) == 0 {
				return globClassSpec{}, 0, false
			}
			return class, i, true
		}
		first = false
		switch {
		case runes[i] == '\\':
			if i+1 >= len(runes) {
				return globClassSpec{}, 0, false
			}
			lo := runes[i+1]
			class.ranges = append(class.ranges, lo, lo)
			i += 2
		case i+2 < len(runes) && runes[i+1] == '-' && runes[i+2] != ']':
			if runes[i] > runes[i+2] {
				return globClassSpec{}, 0, false
			}
			class.ranges = append(class.ranges, runes[i], runes[i+2])
			i += 3
		default:
			class.ranges = append(class.ranges, runes[i], runes[i])
			i++
		}
	}
	return globClassSpec{}, 0, false
}

func (c globClassSpec) matches(r rune) bool {
	inside := false
	for i := 0; i+1 < len(c.ranges); i += 2 {
		if r >= c.ranges[i] && r <= c.ranges[i+1] {
			inside = true
			break
		}
	}
	if c.negated {
		return !inside
	}
	return inside
}

func globMatch(tokens []globToken, name string) bool {
	runes := []rune(name)
	dp := make([]bool, len(runes)+1)
	dp[0] = true
	for _, token := range tokens {
		next := make([]bool, len(runes)+1)
		switch token.kind {
		case globStar:
			reachable := false
			for j := 0; j <= len(runes); j++ {
				if dp[j] {
					reachable = true
				}
				next[j] = reachable
			}
		case globAny:
			for j := 1; j <= len(runes); j++ {
				next[j] = dp[j-1]
			}
		case globLiteral:
			for j := 1; j <= len(runes); j++ {
				next[j] = dp[j-1] && runes[j-1] == token.r
			}
		case globClass:
			for j := 1; j <= len(runes); j++ {
				next[j] = dp[j-1] && token.class.matches(runes[j-1])
			}
		}
		dp = next
	}
	return dp[len(runes)]
}

func scorePattern(tokens []globToken, pattern string) ruleScore {
	score := ruleScore{exact: patternIsExact(pattern)}
	for _, token := range tokens {
		switch token.kind {
		case globLiteral:
			score.literals++
		case globClass:
			score.literals++
		}
	}
	for _, token := range tokens {
		if token.kind != globLiteral {
			break
		}
		score.prefix++
	}
	return score
}

// patternIsExact reports whether the pattern can only match one literal
// string (no wildcard or character class remains after unescaping).
func patternIsExact(pattern string) bool {
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\\':
			i++
		case '*', '?', '[':
			return false
		}
	}
	return true
}
