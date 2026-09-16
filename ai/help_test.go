package ai

import (
	"strings"
	"testing"
)

func TestTokenize_DropsStopWordsAndPunctuation(t *testing.T) {
	got := tokenize("How do I connect my printer?")
	want := []string{"connect", "printer"}
	if len(got) != len(want) {
		t.Fatalf("tokenize = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokenize = %q, want %q", got, want)
		}
	}
}

func TestTokenize_DropsSingleCharacterTokens(t *testing.T) {
	got := tokenize("a b c d e f g h i j k l m n o p q r s t u v w x y z")
	if len(got) != 0 {
		t.Fatalf("tokenize = %q, want empty", got)
	}
}

func TestSearchHelp_FindsConnectionTopicForOfflineSymptom(t *testing.T) {
	hits := SearchHelp("my printer is offline")
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	if hits[0].ID != "connection-problems" {
		t.Fatalf("first hit ID = %q, want %q", hits[0].ID, "connection-problems")
	}
}

func TestSearchHelp_FindsKlipperTopic(t *testing.T) {
	hits := SearchHelp("moonraker url")
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	if hits[0].ID != "add-klipper" {
		t.Fatalf("first hit ID = %q, want %q", hits[0].ID, "add-klipper")
	}
}

func TestSearchHelp_FindsCameraTopic(t *testing.T) {
	hits := SearchHelp("camera feed is black")
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	if hits[0].ID != "cameras" {
		t.Fatalf("first hit ID = %q, want %q", hits[0].ID, "cameras")
	}
}

func TestSearchHelp_NonsenseQueryReturnsNothing(t *testing.T) {
	hits := SearchHelp("zzzqqq xxyyww vvbbaa")
	if hits == nil {
		t.Fatal("expected a non-nil slice")
	}
	if len(hits) != 0 {
		t.Fatalf("expected no hits, got %d", len(hits))
	}
}

func TestSearchHelp_EmptyQueryReturnsEmptyNotNil(t *testing.T) {
	hits := SearchHelp("")
	if hits == nil {
		t.Fatal("expected a non-nil slice")
	}
	if len(hits) != 0 {
		t.Fatalf("expected no hits, got %d", len(hits))
	}
}

func TestSearchHelp_RespectsMaxResults(t *testing.T) {
	hits := SearchHelp("printer")
	if len(hits) > helpMaxResults {
		t.Fatalf("got %d hits, want at most %d", len(hits), helpMaxResults)
	}
}

func TestSearchHelp_IsDeterministic(t *testing.T) {
	first := SearchHelp("printer offline camera")
	second := SearchHelp("printer offline camera")
	if len(first) != len(second) {
		t.Fatalf("hit counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("hit %d ID differs: %q vs %q", i, first[i].ID, second[i].ID)
		}
	}
}

func TestExcerpt_ShortBodyUnchanged(t *testing.T) {
	body := "A short body that is well under the limit."
	got, truncated := Excerpt(body)
	if truncated {
		t.Fatal("expected truncated to be false")
	}
	if got != body {
		t.Fatalf("Excerpt = %q, want unchanged %q", got, body)
	}
}

func TestExcerpt_LongBodyIsTruncatedAndFlagged(t *testing.T) {
	body := strings.Repeat("word ", 300) // 1500 runes, well over the limit
	got, truncated := Excerpt(body)
	if !truncated {
		t.Fatal("expected truncated to be true")
	}
	if !strings.Contains(got, "get_help_topic") {
		t.Fatalf("Excerpt does not contain the notice: %q", got)
	}
}

func TestBestHelpExcerpts_EmptyForNoMatch(t *testing.T) {
	got := BestHelpExcerpts("zzzqqq xxyyww vvbbaa", 3)
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestBestHelpExcerpts_IncludesTopicID(t *testing.T) {
	got := BestHelpExcerpts("moonraker url", 3)
	if !strings.Contains(got, "topic_id:") {
		t.Fatalf("output does not contain topic_id: %q", got)
	}
}
