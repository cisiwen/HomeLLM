package home

import "unicode/utf8"

// Rough token costs. Real tokenizers differ per model, so these err on the high side: ASCII
// text averages ~4 characters per token (counted as 3 here), most other scripts (e.g. Chinese)
// about one character per token, and an image costs 256 (gemma3) to 576 (llava) tokens.
const (
	tokensPerImage   = 600
	tokensPerMessage = 8
)

func estimateTokens(m LLMMessage) int {
	ascii, other := 0, 0
	for _, r := range m.Content {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return ascii/3 + other + tokensPerMessage + len(m.Images)*tokensPerImage
}

// trimHistory drops the oldest messages until the estimated prompt fits in budget tokens. The
// last message (the one being answered) is always kept, even if it alone is over budget - the
// LLM's own error then explains why, which beats silently sending nothing.
func trimHistory(msgs []LLMMessage, budget int) []LLMMessage {
	if budget <= 0 || len(msgs) == 0 {
		return msgs
	}
	total := 0
	start := len(msgs)
	for start > 0 {
		cost := estimateTokens(msgs[start-1])
		if start < len(msgs) && total+cost > budget {
			break
		}
		total += cost
		start--
	}
	// A conversation shouldn't start with the assistant's reply to a dropped question.
	for start < len(msgs)-1 && msgs[start].Role == "assistant" {
		start++
	}
	return msgs[start:]
}
