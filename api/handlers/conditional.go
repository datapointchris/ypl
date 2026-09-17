package handlers

import (
	"strconv"
	"strings"
)

// ifMatch is an If-Match field as RFC 9110 §13.1.1 reads it: "*", which any
// current representation matches, or a list of entity tags.
type ifMatch struct {
	any  bool
	tags []entityTag
}

// entityTag is one entity tag: its opaque value without the quotes, and whether
// it is weak.
type entityTag struct {
	weak   bool
	opaque string
}

// revisionTag is revision as the strong entity tag the server sends.
func revisionTag(revision int64) string {
	return `"` + strconv.FormatInt(revision, 10) + `"`
}

// parseIfMatch reads the If-Match field lines values, each a list, as one
// field. ok is false for a field that is neither "*" nor a list of entity tags.
func parseIfMatch(values []string) (ifMatch, bool) {
	joined := strings.Join(values, ",")
	if strings.TrimSpace(joined) == "*" {
		return ifMatch{any: true}, true
	}
	var field ifMatch
	rest := joined
	for {
		rest = strings.TrimLeft(rest, " \t,")
		if rest == "" {
			return field, len(field.tags) > 0
		}
		var tag entityTag
		tag.weak = strings.HasPrefix(rest, "W/")
		rest = strings.TrimPrefix(rest, "W/")
		opaque, after, ok := quoted(rest)
		if !ok {
			return ifMatch{}, false
		}
		tag.opaque = opaque
		field.tags = append(field.tags, tag)
		rest = strings.TrimLeft(after, " \t")
		if rest != "" && rest[0] != ',' {
			return ifMatch{}, false
		}
	}
}

// quoted is the opaque value of the quoted string s opens with, and what follows
// its closing quote. ok is false when s opens with no quoted string of entity
// tag characters: "!" and "#" through "~", and every byte from 0x80.
func quoted(s string) (opaque, after string, ok bool) {
	if !strings.HasPrefix(s, `"`) {
		return "", "", false
	}
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			return s[1:i], s[i+1:], true
		case c == '!', c >= '#' && c <= '~', c >= 0x80:
		default:
			return "", "", false
		}
	}
	return "", "", false
}

// matches is whether the field matches a current representation whose strong
// entity tag is current, by the strong comparison If-Match uses, under which a
// weak tag matches nothing.
func (field ifMatch) matches(current string) bool {
	if field.any {
		return true
	}
	for _, tag := range field.tags {
		if !tag.weak && `"`+tag.opaque+`"` == current {
			return true
		}
	}
	return false
}
