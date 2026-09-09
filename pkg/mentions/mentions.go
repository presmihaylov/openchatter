// Package mentions parses @name references out of message bodies.
package mentions

import (
	"regexp"
	"sort"
	"strings"
)

var broadcastRe = regexp.MustCompile(`(?i)(^|[^\w@])@(channel|here|everyone)\b`)

// Parse returns the mentioned names present in known, plus whether the body
// contains a broadcast mention (@channel/@here/@everyone). Names may contain
// upper case and spaces, so each known name is matched literally after an @.
func Parse(body string, known map[string]bool) (names []string, broadcast bool) {
	body = stripCode(body)
	broadcast = broadcastRe.MatchString(body)

	type hit struct {
		name string
		pos  int
	}
	var hits []hit
	for name := range known {
		re, err := regexp.Compile(`(^|[^\w@])(@` + regexp.QuoteMeta(name) + `)($|[^\w-])`)
		if err != nil {
			continue
		}
		for _, loc := range re.FindAllStringSubmatchIndex(body, -1) {
			hits = append(hits, hit{name, loc[4]}) // start of the @name group
		}
	}
	// body order; at the same @, the longest name wins ("@John Smith" is
	// Smith's mention, not John's)
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].pos != hits[j].pos {
			return hits[i].pos < hits[j].pos
		}
		return len(hits[i].name) > len(hits[j].name)
	})
	seen := map[string]bool{}
	lastEnd := -1
	for _, h := range hits {
		if h.pos < lastEnd || seen[h.name] {
			continue
		}
		seen[h.name] = true
		lastEnd = h.pos + 1 + len(h.name)
		names = append(names, h.name)
	}
	return names, broadcast
}

var (
	fenceRe        = regexp.MustCompile("(?s)```.*?```")
	inlineRe       = regexp.MustCompile("`[^`\n]*`")
	candidateRe    = regexp.MustCompile(`(^|[^\w@])@([A-Za-z0-9][A-Za-z0-9_-]*)`)
	broadcastWords = map[string]bool{"channel": true, "here": true, "everyone": true}
)

func stripCode(body string) string {
	return inlineRe.ReplaceAllString(fenceRe.ReplaceAllString(body, " "), " ")
}

// Unknown returns the @handles in body that match no known participant, in body
// order, deduped. Code spans and fenced blocks are stripped first, and the
// leading guard skips email addresses, so only real handles are reported. A
// handle that only prefixes a longer known name (@Maria of "@Maria Chen") is
// known, not unknown.
func Unknown(body string, known map[string]bool) []string {
	clean := stripCode(body)

	var out []string
	seen := map[string]bool{}
	for _, loc := range candidateRe.FindAllStringSubmatchIndex(clean, -1) {
		at := loc[4] - 1 // the '@' itself
		handle := clean[loc[4]:loc[5]]
		if broadcastWords[strings.ToLower(handle)] {
			continue
		}
		if known[handle] || startsWithKnownName(clean[at+1:], known) {
			continue
		}
		if seen[handle] {
			continue
		}
		seen[handle] = true
		out = append(out, handle)
	}
	return out
}

// a known name may contain spaces, so a candidate token can be the first word
// of a real mention; treat it as known when a full name matches at that point
func startsWithKnownName(rest string, known map[string]bool) bool {
	for name := range known {
		if !strings.HasPrefix(rest, name) {
			continue
		}
		tail := rest[len(name):]
		if tail == "" {
			return true
		}
		c := tail[0]
		if c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		return true
	}
	return false
}
