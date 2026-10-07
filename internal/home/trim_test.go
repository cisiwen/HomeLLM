package home

import (
	"strings"
	"testing"
)

func TestTrimHistoryKeepsNewestThatFit(t *testing.T) {
	long := strings.Repeat("word ", 600) // ~1000 estimated tokens
	msgs := []LLMMessage{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: long},
		{Role: "user", Content: "second question"},
		{Role: "assistant", Content: long},
		{Role: "user", Content: "third question"},
	}
	got := trimHistory(msgs, 1500)
	if len(got) != 3 || got[0].Content != "second question" || got[2].Content != "third question" {
		t.Fatalf("kept %d messages starting %q", len(got), got[0].Content)
	}
}

func TestTrimHistoryNeverStartsWithAssistant(t *testing.T) {
	msgs := []LLMMessage{
		{Role: "user", Content: strings.Repeat("x", 3000)},
		{Role: "assistant", Content: "short answer"},
		{Role: "user", Content: "next"},
	}
	got := trimHistory(msgs, 100)
	if len(got) != 1 || got[0].Content != "next" {
		t.Fatalf("got %+v", got)
	}
}

func TestTrimHistoryAlwaysKeepsLast(t *testing.T) {
	msgs := []LLMMessage{{Role: "user", Content: strings.Repeat("x", 30000)}}
	if got := trimHistory(msgs, 100); len(got) != 1 {
		t.Fatalf("dropped the only message")
	}
}

func TestEstimateCountsNonASCIIPerCharacter(t *testing.T) {
	if got := estimateTokens(LLMMessage{Content: "你好世界"}); got != 4+tokensPerMessage {
		t.Fatalf("got %d", got)
	}
}
