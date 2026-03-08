package concentrate

import (
	"regexp"
	"strings"
)

var (
	reANSI       = regexp.MustCompile(`\x1b(?:[\x40-\x5A\x5C-\x5F]|\[[\x30-\x3F]*[\x20-\x2F]*[\x40-\x7E])`)
	reTrailSpace = regexp.MustCompile(`[ \t]+\n`)
	reBlankLines = regexp.MustCompile(`\n{3,}`)
	rePromptTail = regexp.MustCompile(`(?i)(?:\[[Yy]/[Nn]\]|\[[Nn]/[Yy]\]|\([Yy]/[Nn]\)|\([Nn]/[Yy]\)|password:|passphrase:|continue\?|proceed\?)\s*$`)
	reNormNum    = regexp.MustCompile(`\b\d+\b`)
	reNormHex    = regexp.MustCompile(`[0-9a-f]{7,}`)
	reNormSpaces = regexp.MustCompile(`\s+`)
)

// normalize strips ANSI codes, normalizes line endings, and collapses blank lines.
func normalize(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = reANSI.ReplaceAllString(text, "")
	text = reTrailSpace.ReplaceAllString(text, "\n")
	text = reBlankLines.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// hasPromptTail returns true if text ends with an interactive prompt pattern.
func hasPromptTail(text string) bool {
	tail := text
	if len(tail) > 256 {
		tail = tail[len(tail)-256:]
	}
	return rePromptTail.MatchString(strings.TrimRight(tail, " \t"))
}

// hasRedrawSignal returns true if text contains terminal redraw sequences.
func hasRedrawSignal(text string) bool {
	return strings.ContainsRune(text, '\r') ||
		strings.Contains(text, "\x1b[2J") ||
		strings.Contains(text, "\x1bc")
}

// structuralSimilarity returns a Jaccard-like score [0,1] comparing
// the structural line signatures of a and b.
func structuralSimilarity(a, b string) float64 {
	left := generateSignature(a)
	right := generateSignature(b)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}

	leftSet := make(map[string]struct{}, len(left))
	for _, l := range left {
		leftSet[l] = struct{}{}
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, r := range right {
		rightSet[r] = struct{}{}
	}

	overlap := 0
	for k := range leftSet {
		if _, ok := rightSet[k]; ok {
			overlap++
		}
	}
	return float64(2*overlap) / float64(len(leftSet)+len(rightSet))
}

func generateSignature(text string) []string {
	lines := strings.Split(normalize(text), "\n")
	result := make([]string, 0, min(len(lines), 24))
	for _, line := range lines {
		if len(result) >= 24 {
			break
		}
		line = strings.ToLower(line)
		line = reNormNum.ReplaceAllString(line, "#")
		line = reNormHex.ReplaceAllString(line, "<hex>")
		line = strings.TrimSpace(reNormSpaces.ReplaceAllString(line, " "))
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

// isBadSummary returns true if candidate is not a useful compression of source.
func isBadSummary(source, candidate string) bool {
	cand := normalize(candidate)
	if cand == "" {
		return true
	}
	lower := strings.ToLower(cand)
	for _, phrase := range []string{"please provide", "wish summarized", "provided command output"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	src := normalize(source)
	if len(src) >= 1024 {
		return len(cand) >= int(float64(len(src))*0.8)
	}
	return src != "" && (cand == src || len(cand) > len(src)+40)
}
