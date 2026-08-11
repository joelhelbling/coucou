package tui

import (
	"os"
	"strings"
)

// Theme is the glyph and colour vocabulary of the display. It exists so the
// view can be rendered identically into a test string and onto a terminal
// that may be missing UTF-8, missing colour, or both.
type Theme struct {
	OK       string
	Fail     string
	Skipped  string
	Timeout  string
	Selected string
	Rule     string
	Spinner  []string
	Colour   bool
	Unicode  bool

	// Ellipsis marks a truncated cell; Sep joins the parts of a composed
	// detail value. Both live here because both must degrade to ASCII.
	Ellipsis string
	Sep      string
}

// UnicodeTheme is the default: braille spinner, box-drawing rule.
func UnicodeTheme(colour bool) Theme {
	return Theme{
		OK:       "✓",
		Fail:     "✗",
		Skipped:  "·",
		Timeout:  "⏱",
		Selected: "▸",
		Rule:     "─",
		Spinner:  []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		Colour:   colour,
		Unicode:  true,
		Ellipsis: "…",
		Sep:      " · ",
	}
}

// ASCIITheme avoids box-drawing and braille for terminals that cannot show
// them. The spinner is the classic four-frame twirl.
func ASCIITheme(colour bool) Theme {
	return Theme{
		OK:       "ok",
		Fail:     "FAIL",
		Skipped:  "-",
		Timeout:  "T/O",
		Selected: ">",
		Rule:     "-",
		Spinner:  []string{"|", "/", "-", "\\"},
		Colour:   colour,
		Unicode:  false,
		Ellipsis: "...",
		Sep:      " | ",
	}
}

// Frame returns the spinner frame for a tick counter. One counter drives
// every spinner on screen so they stay in step.
func (t Theme) Frame(tick int) string {
	if len(t.Spinner) == 0 {
		return ""
	}
	return t.Spinner[tick%len(t.Spinner)]
}

// DetectTheme picks a theme from the environment. forceASCII comes from
// --ascii.
//
// The UTF-8 check is deliberately loose: a locale string is not a promise
// that a font has braille, but an absent or non-UTF-8 locale is a reliable
// signal that it does not.
func DetectTheme(forceASCII bool) Theme {
	colour := os.Getenv("NO_COLOR") == ""

	term := os.Getenv("TERM")
	if term == "" || term == "dumb" {
		return ASCIITheme(false)
	}

	if forceASCII || !utf8Locale() {
		return ASCIITheme(colour)
	}
	return UnicodeTheme(colour)
}

func utf8Locale() bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		v := os.Getenv(k)
		if v == "" {
			continue
		}
		return strings.Contains(strings.ToUpper(v), "UTF-8") ||
			strings.Contains(strings.ToUpper(v), "UTF8")
	}
	return false
}
