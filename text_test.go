package concentrate

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"strips ANSI", "\x1b[32mhello\x1b[0m", "hello"},
		{"normalizes CRLF", "a\r\nb", "a\nb"},
		{"normalizes bare CR", "a\rb", "a\nb"},
		{"collapses blank lines", "a\n\n\n\nb", "a\n\nb"},
		{"strips trailing spaces", "a  \nb  ", "a\nb"},
		{"strips surrounding whitespace", "\n  hello  \n", "hello"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalize(tc.input)
			if got != tc.want {
				t.Errorf("normalize(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestHasPromptTail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  bool
	}{
		{"yn bracket", "Proceed? [y/N] ", true},
		{"Yn bracket", "Continue? [Y/n] ", true},
		{"colon space", "Enter password: ", true},
		{"passphrase", "passphrase: ", true},
		{"bare question mark", "Continue?", true},
		{"proceed question mark", "Proceed?", true},
		{"normal line", "Normal output line", false},
		{"colon not at tail", "Password: was entered\nNext line of output", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hasPromptTail(tc.input)
			if got != tc.want {
				t.Errorf("hasPromptTail(%q) = %v; want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestHasRedrawSignal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  bool
	}{
		{"bare CR", "progress\r100%", true},
		{"erase display", "\x1b[2J", true},
		{"reset", "\x1bc", true},
		{"plain output", "plain output\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hasRedrawSignal(tc.input)
			if got != tc.want {
				t.Errorf("hasRedrawSignal(%q) = %v; want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestStructuralSimilarity(t *testing.T) {
	t.Run("identical", func(t *testing.T) {
		text := "foo bar\nbaz qux"
		got := structuralSimilarity(text, text)
		if got != 1.0 {
			t.Errorf("got %f; want 1.0", got)
		}
	})

	t.Run("no overlap", func(t *testing.T) {
		got := structuralSimilarity("alpha beta", "gamma delta epsilon")
		if got != 0.0 {
			t.Errorf("got %f; want 0.0", got)
		}
	})

	t.Run("empty inputs", func(t *testing.T) {
		if structuralSimilarity("", "foo") != 0.0 {
			t.Error("expected 0 for empty left")
		}
		if structuralSimilarity("foo", "") != 0.0 {
			t.Error("expected 0 for empty right")
		}
	})

	t.Run("number normalization", func(t *testing.T) {
		a := "processed 100 files"
		b := "processed 999 files"
		if got := structuralSimilarity(a, b); got != 1.0 {
			t.Errorf("got %f; want 1.0 (numbers should be normalized)", got)
		}
	})

	t.Run("hex normalization", func(t *testing.T) {
		a := "commit abc1234def"
		b := "commit 999beef00"
		if got := structuralSimilarity(a, b); got != 1.0 {
			t.Errorf("got %f; want 1.0 (hex should be normalized)", got)
		}
	})

	t.Run("partial overlap is between 0 and 1", func(t *testing.T) {
		sim := structuralSimilarity("foo bar\nbaz", "foo bar\nqux")
		if sim <= 0.0 || sim >= 1.0 {
			t.Errorf("got %f; want (0, 1)", sim)
		}
	})
}

func TestIsBadSummary(t *testing.T) {
	t.Run("empty candidate", func(t *testing.T) {
		if !isBadSummary("source", "") {
			t.Error("empty candidate should be bad")
		}
	})

	for _, phrase := range []string{"please provide", "wish summarized", "provided command output"} {
		phrase := phrase
		t.Run("refusal: "+phrase, func(t *testing.T) {
			if !isBadSummary("some output", phrase) {
				t.Errorf("refusal phrase %q should be detected as bad", phrase)
			}
		})
	}

	t.Run("candidate equals short source", func(t *testing.T) {
		src := "short text"
		if !isBadSummary(src, src) {
			t.Error("same-as-source should be bad for short input")
		}
	})

	t.Run("candidate longer than short source", func(t *testing.T) {
		src := "short"
		if !isBadSummary(src, strings.Repeat("x", len(src)+50)) {
			t.Error("longer-than-source should be bad")
		}
	})

	t.Run("good summary", func(t *testing.T) {
		src := "Tests: 42 passed, 0 failed, 0 skipped in 3.14s"
		if isBadSummary(src, "All 42 tests passed.") {
			t.Error("good summary should not be bad")
		}
	})

	t.Run("large source — candidate at 70% is ok", func(t *testing.T) {
		src := strings.Repeat("x", 2000)
		cand := strings.Repeat("x", 1400) // 70% — below 80% threshold
		if isBadSummary(src, cand) {
			t.Error("70% of large source should be acceptable")
		}
	})

	t.Run("large source — candidate at 90% is bad", func(t *testing.T) {
		src := strings.Repeat("x", 2000)
		cand := strings.Repeat("x", 1800) // 90% — above 80% threshold
		if !isBadSummary(src, cand) {
			t.Error("90% of large source should be bad")
		}
	})
}
