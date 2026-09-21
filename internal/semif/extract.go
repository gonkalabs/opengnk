package semif

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

const missingLogprobPenalty = 20.0

type chatLogprobToken struct {
	Token       string          `json:"token"`
	Logprob     float64         `json:"logprob"`
	TopLogprobs []topLogprobHit `json:"top_logprobs"`
}

type topLogprobHit struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

type completionsLogprobs struct {
	Tokens        []string             `json:"tokens"`
	TokenLogprobs []*float64           `json:"token_logprobs"`
	TopLogprobs   []map[string]float64 `json:"top_logprobs"`
}

// ExtractLogits reads A–P scores from an OpenAI chat or completions body.
func ExtractLogits(body []byte, optionCount int) (logits []float64, sampled string, usage Usage, missing []string, err error) {
	if optionCount < 2 || optionCount > len(Letters) {
		return nil, "", Usage{}, nil, fmt.Errorf("option count out of range")
	}

	var parsed struct {
		Usage   *Usage `json:"usage"`
		Choices []struct {
			Text    string `json:"text"`
			Message *struct {
				Content string `json:"content"`
			} `json:"message"`
			Logprobs json.RawMessage `json:"logprobs"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", Usage{}, nil, fmt.Errorf("decode scorer response: %w", err)
	}
	if parsed.Usage != nil {
		usage = *parsed.Usage
	}
	if len(parsed.Choices) == 0 {
		return nil, "", usage, nil, fmt.Errorf("scorer returned no choices")
	}
	choice := parsed.Choices[0]
	if choice.Message != nil {
		sampled = NormalizeSlot(choice.Message.Content)
	}
	if sampled == "" {
		sampled = NormalizeSlot(choice.Text)
	}

	scores := map[string]float64{}
	if len(choice.Logprobs) > 0 && string(choice.Logprobs) != "null" {
		collectChatLogprobs(choice.Logprobs, scores)
		collectCompletionsLogprobs(choice.Logprobs, scores)
	}
	if sampled != "" {
		if _, ok := scores[sampled]; !ok {
			// Sampled letter is known to have been generated; give it a
			// neutral anchor if the server omitted top_logprobs.
			scores[sampled] = 0
		}
	}

	logits = make([]float64, optionCount)
	observedMin := math.Inf(1)
	for _, v := range scores {
		if v < observedMin {
			observedMin = v
		}
	}
	if math.IsInf(observedMin, 1) {
		observedMin = 0
	}
	for i := 0; i < optionCount; i++ {
		letter := LetterAt(i)
		if v, ok := scores[letter]; ok {
			logits[i] = v
			continue
		}
		logits[i] = observedMin - missingLogprobPenalty
		missing = append(missing, letter)
	}
	if len(missing) == optionCount {
		return nil, sampled, usage, missing, fmt.Errorf("scorer returned no option-letter logprobs")
	}
	return logits, sampled, usage, missing, nil
}

func collectChatLogprobs(raw json.RawMessage, scores map[string]float64) {
	var chat struct {
		Content []chatLogprobToken `json:"content"`
	}
	if json.Unmarshal(raw, &chat) != nil || len(chat.Content) == 0 {
		return
	}
	first := chat.Content[0]
	if slot := NormalizeSlot(first.Token); slot != "" {
		scores[slot] = first.Logprob
	}
	for _, hit := range first.TopLogprobs {
		if slot := NormalizeSlot(hit.Token); slot != "" {
			if _, exists := scores[slot]; !exists {
				scores[slot] = hit.Logprob
			}
		}
	}
}

func collectCompletionsLogprobs(raw json.RawMessage, scores map[string]float64) {
	var comp completionsLogprobs
	if json.Unmarshal(raw, &comp) != nil {
		return
	}
	if len(comp.Tokens) > 0 {
		if slot := NormalizeSlot(comp.Tokens[0]); slot != "" && len(comp.TokenLogprobs) > 0 && comp.TokenLogprobs[0] != nil {
			scores[slot] = *comp.TokenLogprobs[0]
		}
	}
	if len(comp.TopLogprobs) == 0 {
		return
	}
	for token, lp := range comp.TopLogprobs[0] {
		if slot := NormalizeSlot(token); slot != "" {
			if _, exists := scores[slot]; !exists {
				scores[slot] = lp
			}
		}
	}
}

// DegradedLogits builds a one-hot-ish score vector from a sampled letter.
func DegradedLogits(optionCount int, sampled string) ([]float64, []string, error) {
	if optionCount < 2 || optionCount > len(Letters) {
		return nil, nil, fmt.Errorf("option count out of range")
	}
	sampled = NormalizeSlot(sampled)
	idx := strings.Index(Letters[:optionCount], sampled)
	if idx < 0 {
		return nil, nil, fmt.Errorf("sampled token %q is not a declared option letter", sampled)
	}
	logits := make([]float64, optionCount)
	missing := make([]string, 0, optionCount-1)
	for i := 0; i < optionCount; i++ {
		if i == idx {
			logits[i] = 0
			continue
		}
		logits[i] = -missingLogprobPenalty
		missing = append(missing, LetterAt(i))
	}
	return logits, missing, nil
}
