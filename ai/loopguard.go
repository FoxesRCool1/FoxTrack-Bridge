package ai

import "strings"

// The second line of defence against a runaway reply, after CollapseRepetition.
//
// CollapseRepetition drops lines that are IDENTICAL. That catches a model stuck
// on one sentence, and misses the commoner failure: a model that has finished
// answering and cannot stop signing off, writing a different closing line every
// time. Observed on a 27B local model asked how to add a printer — the answer
// was correct and complete, then forty paragraphs of "That's it, let me know if
// you need anything else", each one worded slightly differently, running until
// the token cap.
//
// Exact matching cannot see that, and similarity scoring on whole lines scores
// those closings at around 0.5, which is also where genuinely different
// sentences sit — there is no threshold that separates them.
//
// Word trigrams do separate them. A line that adds nothing new is one whose
// word trigrams have almost all appeared already, whatever order they are
// rearranged into. Real new content — a step, a warning, a temperature — brings
// new trigrams with it.

const (
	// A line must be at least this many words before it is judged. Short lines
	// ("Then:", "Good luck!") share trigrams honestly.
	runawayMinWords = 8
	// The fraction of a line's trigrams that must already have been seen for it
	// to count as adding nothing.
	runawayStaleRatio = 0.6
	// How many such lines are tolerated before the reply is cut. Two is a
	// writer restating a caveat; three is a loop.
	runawayAllowance = 2
	// How many leading words make up a line's opening, for the second signal
	// below.
	runawayPrefixWords = 4
)

// prefix returns a line's normalized opening words, or "" when the line is too
// short to have one.
//
// This is the second signal, and it is the one that catches the sign-off loop.
// Trigram overlap alone does not: a model rewording the same sentence five
// different ways scores about 0.45, which is also where genuinely different
// sentences sit. What those rewordings DO share is how they start — "if you run
// into...", "that's the full...", "if the printer..." — because the opening is
// what the loop is anchored on. Real prose almost never opens two substantial
// lines with the same four words unless it is restating itself, and when it
// does, the allowance covers it.
func prefix(line string) string {
	words := strings.Fields(repeatKey(line))
	if len(words) < runawayPrefixWords {
		return ""
	}
	return strings.Join(words[:runawayPrefixWords], " ")
}

// trigrams returns the word trigrams of one line, normalized.
func trigrams(line string) []string {
	words := strings.Fields(repeatKey(line))
	if len(words) < 3 {
		return nil
	}
	out := make([]string, 0, len(words)-2)
	for i := 0; i+3 <= len(words); i++ {
		out = append(out, words[i]+" "+words[i+1]+" "+words[i+2])
	}
	return out
}

// TrimRunaway cuts a reply at the point it stops adding anything and starts
// looping, and says so. Everything before that point is left exactly as it was:
// a reply that loops at the end usually answered the question first, and
// throwing the answer away to punish the tail would be worse than the tail.
//
// Returns text unchanged when no loop is found, which is the normal case for
// any model that is not misbehaving.
func TrimRunaway(text string) string {
	lines := strings.Split(text, "\n")
	seen := make(map[string]bool)
	seenPrefix := make(map[string]bool)

	stale := 0
	cutAt := -1

	for i, line := range lines {
		grams := trigrams(line)
		if len(strings.Fields(repeatKey(line))) < runawayMinWords || len(grams) == 0 {
			// Too short to judge, but its trigrams still count as seen, so a
			// loop built out of short lines cannot hide behind the threshold.
			for _, g := range grams {
				seen[g] = true
			}
			continue
		}

		old := 0
		for _, g := range grams {
			if seen[g] {
				old++
			}
		}
		for _, g := range grams {
			seen[g] = true
		}

		opening := prefix(line)
		repeatedOpening := opening != "" && seenPrefix[opening]
		seenPrefix[opening] = true

		if !repeatedOpening && float64(old)/float64(len(grams)) < runawayStaleRatio {
			// This line brought something new, so whatever came before it was
			// not a loop after all.
			stale = 0
			cutAt = -1
			continue
		}

		stale++
		if cutAt < 0 {
			cutAt = i
		}
		if stale > runawayAllowance {
			break
		}
	}

	if stale <= runawayAllowance || cutAt < 0 {
		return text
	}

	trimmed := strings.TrimSpace(strings.Join(lines[:cutAt], "\n"))
	if trimmed == "" {
		// The whole reply was the loop. Better to hand back what there is than
		// nothing at all; the caller turns an empty reply into an error.
		return text
	}
	// Told, not hidden. A reply that was quietly edited is a reply the user
	// cannot judge, and "the model started repeating itself" is worth knowing
	// when you are choosing which model to point the Bridge at.
	return trimmed + "\n\n_(This reply was cut short: the assistant started repeating itself without adding anything new.)_"
}
