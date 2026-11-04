package emailhtml

import (
	"regexp"
	"strconv"
	"strings"
)

// Lengths are bounded by property, so a message can neither move over the interface
// nor grow out of proportion. Every value also passes safeStyleValue.

var lengthToken = regexp.MustCompile(`^([+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+))(px|pt|em|rem|%)?$`)

// bounds limits a length per unit (zero refuses it); minPx is the lowest value in px,
// unitless the largest plain number.
type bounds struct {
	minPx, px, pt, em, pct, unitless float64
}

func (b bounds) allows(token string) bool {
	m := lengthToken.FindStringSubmatch(token)
	if m == nil {
		return false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return false
	}

	var limit, px float64
	switch m[2] {
	case "":
		if v == 0 {
			return true
		}
		return b.unitless > 0 && v > 0 && v <= b.unitless
	case "%":
		if b.pct == 0 {
			return false
		}
		low := 0.0
		if b.minPx < 0 {
			low = -b.pct
		}
		return v >= low && v <= b.pct
	case "px":
		limit, px = b.px, v
	case "pt":
		limit, px = b.pt, v*4/3
	default: // em, rem
		limit, px = b.em, v*16
	}

	return limit > 0 && v <= limit && px >= b.minPx
}

var (
	offsetBounds    = bounds{px: 200, pt: 150, em: 12.5, pct: 100}
	sizeBounds      = bounds{px: 2000, pt: 1500, em: 125, pct: 100}
	borderBounds    = bounds{px: 20, pt: 15, em: 1.25}
	radiusBounds    = bounds{px: 100, pt: 75, em: 6.25, pct: 50}
	fontSizeBounds  = bounds{px: 72, pt: 54, em: 5, pct: 500}
	lineHeightBound = bounds{px: 100, pt: 75, em: 5, pct: 500, unitless: 5}
	spacingBounds   = bounds{minPx: -5, px: 50, pt: 37.5, em: 3}
	alignBounds     = bounds{minPx: -10, px: 10, pt: 7.5, em: 0.625}
)

var (
	borderWidthWords = []string{"thin", "medium", "thick"}
	fontSizeWords    = []string{"xx-small", "x-small", "small", "medium", "large", "x-large", "xx-large", "smaller", "larger"}
	alignWords       = []string{"baseline", "sub", "super", "text-top", "text-bottom", "middle", "top", "bottom"}
)

// cssTokens splits a value on spaces outside parentheses.
func cssTokens(value string) []string {
	var tokens []string
	depth, start := 0, -1
	for i, r := range value {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		}
		space := r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f'
		switch {
		case space && depth == 0 && start >= 0:
			tokens = append(tokens, value[start:i])
			start = -1
		case !space && start < 0:
			start = i
		}
	}
	if start >= 0 {
		tokens = append(tokens, value[start:])
	}

	return tokens
}

func isKeyword(token string, words []string) bool {
	for _, w := range words {
		if token == w {
			return true
		}
	}

	return false
}

// looksLikeLength tells a token that must be checked as a length from a
// keyword, a color or a font name.
func looksLikeLength(token string) bool {
	return token != "" && strings.ContainsRune("0123456789.+-", rune(token[0]))
}

// lengths accepts one to max lengths within b or keywords.
func lengths(max int, b bounds, words ...string) func(string) bool {
	return func(value string) bool {
		if !safeStyleValue(value) {
			return false
		}
		tokens := cssTokens(strings.ToLower(value))
		if len(tokens) == 0 || len(tokens) > max {
			return false
		}
		for _, t := range tokens {
			if !isKeyword(t, words) && !b.allows(t) {
				return false
			}
		}

		return true
	}
}

// shorthand accepts any keywords and colors but checks every length within b.
func shorthand(b bounds) func(string) bool {
	return func(value string) bool {
		if !safeStyleValue(value) {
			return false
		}
		for _, t := range cssTokens(strings.ToLower(value)) {
			if t != "/" && looksLikeLength(t) && !b.allows(t) {
				return false
			}
		}

		return true
	}
}

// fontShorthand checks the size, the line height after "/" and a numeric
// weight of the font shorthand.
func fontShorthand(value string) bool {
	if !safeStyleValue(value) {
		return false
	}
	for _, t := range cssTokens(strings.ToLower(value)) {
		if !looksLikeLength(t) {
			continue
		}
		size, height, slash := strings.Cut(t, "/")
		if slash {
			if !fontSizeBounds.allows(size) || !lineHeightBound.allows(height) {
				return false
			}
			continue
		}
		if weight, err := strconv.Atoi(t); err == nil {
			if weight < 1 || weight > 1000 {
				return false
			}
			continue
		}
		if !fontSizeBounds.allows(t) {
			return false
		}
	}

	return true
}

// boundedStyles maps the properties with a length to their check; the other
// allowed properties take keywords and colors only.
var boundedStyles = map[string]func(string) bool{
	"margin": lengths(4, offsetBounds, "auto"), "margin-top": lengths(1, offsetBounds, "auto"),
	"margin-right": lengths(1, offsetBounds, "auto"), "margin-bottom": lengths(1, offsetBounds, "auto"),
	"margin-left": lengths(1, offsetBounds, "auto"),
	"padding":     lengths(4, offsetBounds), "padding-top": lengths(1, offsetBounds),
	"padding-right": lengths(1, offsetBounds), "padding-bottom": lengths(1, offsetBounds),
	"padding-left": lengths(1, offsetBounds),
	"text-indent":  lengths(1, offsetBounds), "border-spacing": lengths(2, offsetBounds),
	"width": lengths(1, sizeBounds, "auto"), "height": lengths(1, sizeBounds, "auto"),
	"min-width": lengths(1, sizeBounds, "auto"), "min-height": lengths(1, sizeBounds, "auto"),
	"max-width": lengths(1, sizeBounds, "none"), "max-height": lengths(1, sizeBounds, "none"),
	"border-width": lengths(4, borderBounds, borderWidthWords...),
	"border":       shorthand(borderBounds), "border-top": shorthand(borderBounds),
	"border-right": shorthand(borderBounds), "border-bottom": shorthand(borderBounds),
	"border-left":    shorthand(borderBounds),
	"border-radius":  shorthand(radiusBounds),
	"font-size":      lengths(1, fontSizeBounds, fontSizeWords...),
	"font":           fontShorthand,
	"line-height":    lengths(1, lineHeightBound, "normal"),
	"letter-spacing": lengths(1, spacingBounds, "normal"),
	"word-spacing":   lengths(1, spacingBounds, "normal"),
	"vertical-align": lengths(1, alignBounds, alignWords...),
}
