package agent

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"twinwright/internal/redact"
)

// minMaskedSecret is the shortest passed value that is masked. It mirrors the
// floor in internal/redact, which keeps ordinary words from being masked.
const minMaskedSecret = 8

// redacted is what a masked value is replaced with, taken from the redact
// package so that there is one spelling of it.
var redacted = redact.Secrets("12345678", "12345678")

// secretSpellings lists the ways a passed value can be written in text: as it
// is, escaped as a JSON string (also with the "\/" some encoders write), and
// escaped as a Go quoted string. An agent's own logging picks one, and the error
// text quotes whatever it kept, so a mask that looks for the raw value alone
// misses every value holding a quote, a backslash or a control character.
//
// The longest spelling comes first, so that one that contains another is masked
// whole. Values under the floor have no spellings, as they are never masked.
func secretSpellings(secrets []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(spelling string) {
		if !seen[spelling] {
			seen[spelling] = true
			out = append(out, spelling)
		}
	}
	for _, secret := range secrets {
		if len(secret) < minMaskedSecret {
			continue
		}
		spellings := []string{secret}
		if encoded, err := json.Marshal(secret); err == nil {
			spellings = append(spellings, string(encoded[1:len(encoded)-1]))
		}
		quoted := strconv.Quote(secret)
		spellings = append(spellings, quoted[1:len(quoted)-1])
		for _, spelling := range spellings {
			add(spelling)
			if strings.Contains(spelling, "/") {
				add(strings.ReplaceAll(spelling, "/", `\/`))
			}
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// maskSpellings replaces every spelling in s.
func maskSpellings(s string, spellings []string) string {
	for _, spelling := range spellings {
		s = strings.ReplaceAll(s, spelling, redacted)
	}
	return s
}
