package main

import (
	"bytes"
	"encoding/json"
)

// The stream interceptor sees the exact bytes the host forwards downstream, so
// the payload is already in the client's protocol rather than the provider's.
// Parsing therefore keys off the response shape, not the provider name, and a
// single parser handles every dialect that embeds a usage object.

const (
	// maxLineBytes bounds a single buffered SSE line. A line longer than this is
	// discarded rather than retained, so one pathological upstream cannot pin
	// memory per in-flight request.
	maxLineBytes = 1 << 20
)

// streamParser incrementally consumes SSE bytes for one request and keeps a
// running estimate of input and output tokens.
type streamParser struct {
	line []byte
	// overflow marks a line already too long; its remainder is skipped.
	overflow bool

	// text is the count of characters of assistant-visible output seen so far.
	text int64
	// reported caches the last usage object an upstream sent, which is what
	// OpenAI-compatible providers attach at the end of a stream.
	reported usageDetail
}

// feed consumes one chunk. The chunk is read-only and never retained beyond the
// incomplete trailing line.
func (p *streamParser) feed(chunk []byte) {
	for len(chunk) > 0 {
		idx := bytes.IndexByte(chunk, '\n')
		if idx < 0 {
			p.appendPartial(chunk)
			return
		}
		part := chunk[:idx]
		chunk = chunk[idx+1:]
		if p.overflow {
			// Finish skipping the overlong line, then resume normally.
			p.overflow = false
			p.line = p.line[:0]
			continue
		}
		if len(p.line) > 0 {
			p.line = append(p.line, part...)
			p.observeLine(p.line)
			p.line = p.line[:0]
			continue
		}
		p.observeLine(part)
	}
}

func (p *streamParser) appendPartial(chunk []byte) {
	if p.overflow {
		return
	}
	if len(p.line)+len(chunk) > maxLineBytes {
		p.overflow = true
		p.line = p.line[:0]
		return
	}
	p.line = append(p.line, chunk...)
}

// finish consumes a trailing line the upstream sent without a newline.
func (p *streamParser) finish() {
	if !p.overflow && len(p.line) > 0 {
		p.observeLine(p.line)
	}
	p.line = nil
	p.overflow = false
}

// observeLine handles one complete SSE line.
func (p *streamParser) observeLine(raw []byte) {
	line := bytes.TrimSuffix(raw, []byte("\r"))
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] == ':' {
		return
	}
	payload := line
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if len(payload) == 0 || payload[0] != '{' {
		// "[DONE]" and bare event names carry no usage.
		return
	}
	p.observeJSON(payload)
}

func (p *streamParser) observeJSON(payload []byte) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(payload, &event); err != nil {
		return
	}

	// A usage object can sit at the top level (OpenAI include_usage, Claude
	// message_delta, Gemini usageMetadata) or under "response" for wrapped
	// events. Check both without a second decode.
	if detail, ok := usageFromObject(event); ok {
		p.mergeReported(detail)
		return
	}
	// Claude's message_start nests usage inside the message object, and its
	// message_delta later carries output_tokens only.
	if raw, ok := event["message"]; ok && len(raw) > 0 && raw[0] == '{' {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err == nil {
			if detail, okUsage := usageFromObject(inner); okUsage {
				p.mergeReported(detail)
				return
			}
		}
	}

	p.observeTextDelta(payload, event)
}

// usageFromObject finds a usage object in an event, either directly or under a
// wrapped "response".
func usageFromObject(obj map[string]json.RawMessage) (usageDetail, bool) {
	for _, key := range []string{"usage", "usageMetadata", "usage_metadata"} {
		if raw, ok := obj[key]; ok {
			if detail, okParse := parseUsageObject(raw); okParse {
				return detail, true
			}
		}
	}
	if raw, ok := obj["response"]; ok && len(raw) > 0 && raw[0] == '{' {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err == nil {
			for _, key := range []string{"usage", "usageMetadata", "usage_metadata"} {
				if rawUsage, okUsage := inner[key]; okUsage {
					if detail, okParse := parseUsageObject(rawUsage); okParse {
						return detail, true
					}
				}
			}
		}
	}
	return usageDetail{}, false
}

// mergeReported folds a newly observed usage object into the running one.
// Providers split counters across events (Anthropic reports input at
// message_start and output at message_delta) or grow them cumulatively
// (Gemini), so a whole-object replace would either lose a counter or move it
// backwards. Every counter is monotonic within one stream, so the greater value
// always wins.
func (p *streamParser) mergeReported(src usageDetail) {
	dst := &p.reported
	if src.InputTokens > dst.InputTokens {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > dst.OutputTokens {
		dst.OutputTokens = src.OutputTokens
	}
	if src.ReasoningTokens > dst.ReasoningTokens {
		dst.ReasoningTokens = src.ReasoningTokens
	}
	if src.CachedTokens > dst.CachedTokens {
		dst.CachedTokens = src.CachedTokens
	}
	if src.CacheReadTokens > dst.CacheReadTokens {
		dst.CacheReadTokens = src.CacheReadTokens
	}
	if src.CacheCreationTokens > dst.CacheCreationTokens {
		dst.CacheCreationTokens = src.CacheCreationTokens
	}
	if src.TotalTokens > dst.TotalTokens {
		dst.TotalTokens = src.TotalTokens
	}
	if sum := dst.InputTokens + dst.OutputTokens; sum > dst.TotalTokens {
		dst.TotalTokens = sum
	}
}

// observeTextDelta adds the characters of newly emitted text to the estimate.
// It runs only when the event carries no usage object, since a usage-bearing
// event is a cumulative snapshot rather than new content.
func (p *streamParser) observeTextDelta(payload []byte, event map[string]json.RawMessage) {
	// OpenAI chat.completions and responses: choices[].delta.content,
	// choices[].text, and delta.output_text for newer shapes.
	if raw, ok := event["choices"]; ok {
		p.observeChoices(raw)
		return
	}
	// Claude messages: content_block_delta.delta.text or .thinking, and
	// content_block_start.content_block.text on the first block.
	if raw, ok := event["delta"]; ok {
		if text, okText := jsonStringField(raw, "text"); okText {
			p.text += int64(len(text))
			return
		}
		if text, okText := jsonStringField(raw, "thinking"); okText {
			p.text += int64(len(text))
			return
		}
	}
	if raw, ok := event["content_block"]; ok {
		if text, okText := jsonStringField(raw, "text"); okText {
			p.text += int64(len(text))
			return
		}
	}
	// Gemini candidates: candidates[].content.parts[].text.
	if raw, ok := event["candidates"]; ok {
		p.observeCandidates(raw)
		return
	}
	if raw, ok := event["response"]; ok && len(raw) > 0 && raw[0] == '{' {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err == nil {
			if choices, okChoices := inner["choices"]; okChoices {
				p.observeChoices(choices)
				return
			}
			if candidates, okCandidates := inner["candidates"]; okCandidates {
				p.observeCandidates(candidates)
				return
			}
		}
	}
	// A non-streaming response body arriving as one chunk carries its text in
	// the top-level message/content fields.
	if raw, ok := event["message"]; ok {
		if text, okText := jsonStringField(raw, "content"); okText {
			p.text += int64(len(text))
		}
	}
}

func (p *streamParser) observeChoices(raw json.RawMessage) {
	var choices []struct {
		Delta struct {
			Content    *string `json:"content"`
			Reasoning  *string `json:"reasoning_content"`
			OutputText *string `json:"output_text"`
		} `json:"delta"`
		Text *string `json:"text"`
		// Non-streaming shape.
		Message struct {
			Content *string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &choices); err != nil {
		return
	}
	for _, choice := range choices {
		if choice.Delta.Content != nil {
			p.text += int64(len(*choice.Delta.Content))
		}
		if choice.Delta.Reasoning != nil {
			p.text += int64(len(*choice.Delta.Reasoning))
		}
		if choice.Delta.OutputText != nil {
			p.text += int64(len(*choice.Delta.OutputText))
		}
		if choice.Text != nil {
			p.text += int64(len(*choice.Text))
		}
		if choice.Message.Content != nil {
			p.text += int64(len(*choice.Message.Content))
		}
	}
}

func (p *streamParser) observeCandidates(raw json.RawMessage) {
	var candidates []struct {
		Content struct {
			Parts []struct {
				Text *string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &candidates); err != nil {
		return
	}
	for _, candidate := range candidates {
		for _, part := range candidate.Content.Parts {
			if part.Text != nil {
				p.text += int64(len(*part.Text))
			}
		}
	}
}

// parseUsageObject reads a usage object from any supported dialect. Providers
// disagree on which counters are inclusive, so the raw values are kept and the
// headline total is computed conservatively.
func parseUsageObject(raw json.RawMessage) (usageDetail, bool) {
	var usage struct {
		// OpenAI
		PromptTokens        *int64 `json:"prompt_tokens"`
		CompletionTokens    *int64 `json:"completion_tokens"`
		TotalTokens         *int64 `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens *int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens *int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`

		// Anthropic
		InputTokens              *int64 `json:"input_tokens"`
		OutputTokens             *int64 `json:"output_tokens"`
		CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`

		// Gemini
		PromptTokenCount        *int64 `json:"promptTokenCount"`
		CandidatesTokenCount    *int64 `json:"candidatesTokenCount"`
		TotalTokenCount         *int64 `json:"totalTokenCount"`
		CachedContentTokenCount *int64 `json:"cachedContentTokenCount"`
		ThoughtsTokenCount      *int64 `json:"thoughtsTokenCount"`
	}
	if err := json.Unmarshal(raw, &usage); err != nil {
		return usageDetail{}, false
	}

	var detail usageDetail
	haveAny := false
	set := func(dst *int64, src *int64) {
		if src != nil && *src > 0 {
			*dst = *src
			haveAny = true
		}
	}
	// Gemini and OpenAI both use "prompt"; Anthropic uses "input". Prefer
	// whichever is present so one merge handles all three.
	if usage.PromptTokens != nil {
		set(&detail.InputTokens, usage.PromptTokens)
	} else {
		set(&detail.InputTokens, usage.InputTokens)
	}
	if usage.CompletionTokens != nil {
		set(&detail.OutputTokens, usage.CompletionTokens)
	} else {
		set(&detail.OutputTokens, usage.OutputTokens)
	}
	if usage.PromptTokenCount != nil {
		set(&detail.InputTokens, usage.PromptTokenCount)
	}
	if usage.CandidatesTokenCount != nil {
		set(&detail.OutputTokens, usage.CandidatesTokenCount)
	}
	if usage.PromptTokensDetails != nil {
		set(&detail.CachedTokens, usage.PromptTokensDetails.CachedTokens)
	}
	set(&detail.CachedTokens, usage.CachedContentTokenCount)
	set(&detail.CachedTokens, usage.CacheReadInputTokens)
	set(&detail.CacheCreationTokens, usage.CacheCreationInputTokens)
	if usage.CompletionTokensDetails != nil {
		set(&detail.ReasoningTokens, usage.CompletionTokensDetails.ReasoningTokens)
	}
	set(&detail.ReasoningTokens, usage.ThoughtsTokenCount)

	switch {
	case usage.TotalTokens != nil && *usage.TotalTokens > 0:
		detail.TotalTokens = *usage.TotalTokens
	case usage.TotalTokenCount != nil && *usage.TotalTokenCount > 0:
		detail.TotalTokens = *usage.TotalTokenCount
	default:
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens
	}
	return detail, haveAny
}

// jsonStringField extracts a string field from a raw JSON object.
func jsonStringField(raw json.RawMessage, name string) (string, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return "", false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", false
	}
	value, ok := obj[name]
	if !ok || len(value) == 0 || value[0] != '"' {
		return "", false
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return "", false
	}
	return text, true
}
