package handlers

import (
	"fmt"
	"strings"
)

// reach is how far a reference is allowed to reach past the exact forms.
type reach int

const (
	// exactly resolves an id, a whole title, and a title whose slug is the
	// whole of what was sent. Nothing else.
	exactly reach = iota
	// loosely also resolves a title merely holding what was sent.
	loosely
)

// titled is a row a reference names by its title: its id, and the title.
type titled struct{ id, title string }

// resolveTitled is the id of the row among rows that ref names, looking as far
// as how says. name is what the error calls ref. The caller has already tried
// ref as an id, which is a keyed read rather than a walk of every row.
//
// The exact forms are the title as written and the title as a slug — the slug
// so that case, spacing and punctuation do not have to be reproduced, and the
// title as written first so that Deep House and Deep-House each stay reachable
// by their own text although they slug alike.
//
// Holding what was sent is the last resort, and which verbs are offered it is
// each caller's to say. A read of a playlist, its videos, its suggestions or a
// video resolves loosely. A rename, a delete, the read of an order and the
// write of one resolve exactly. The order is read exactly because the edit it
// seeds is written exactly, and a read a write refuses buys an editing session
// that is then thrown away.
//
// The ambiguity refusal is what makes loose matching safe, and it fires only on
// two matches — a single *wrong* match is unambiguous, so it resolves cleanly
// to a row nobody named. That costs a read another read, and it costs a delete
// the playlist. It is a guard that weakens as the rows thin: among one
// playlist, any one letter reaches it.
func resolveTitled(rows []titled, name, ref string, how reach) (string, error) {
	matching := func(matches func(titled) bool) []titled {
		var found []titled
		for _, row := range rows {
			if matches(row) {
				found = append(found, row)
			}
		}
		return found
	}
	found := matching(func(row titled) bool { return row.title == ref })
	if len(found) == 0 && slug(ref) != "" {
		found = matching(func(row titled) bool { return slug(row.title) == slug(ref) })
	}
	if len(found) == 0 && slug(ref) != "" {
		holding := matching(func(row titled) bool { return strings.Contains(slug(row.title), slug(ref)) })
		// An exact resolution that found nothing is told which titles hold what
		// it sent, rather than only that nothing answered to it. Somebody who
		// shortened a title is otherwise sent looking for a row they are already
		// looking at.
		if how == exactly && len(holding) > 0 {
			return "", referenceError{name: name, value: ref, nearby: titlesOf(holding)}
		}
		if how == loosely {
			found = holding
		}
	}
	switch len(found) {
	case 0:
		return "", referenceError{name: name, value: ref}
	case 1:
		return found[0].id, nil
	}
	return "", referenceError{name: name, value: ref, candidates: titlesOf(found)}
}

// titlesOf names each row the way a refusal names it: the title somebody typed
// part of, and the id that reaches it whatever the title is.
func titlesOf(rows []titled) []string {
	named := make([]string, len(rows))
	for i, row := range rows {
		named[i] = fmt.Sprintf("%q (%s)", row.title, row.id)
	}
	return named
}
