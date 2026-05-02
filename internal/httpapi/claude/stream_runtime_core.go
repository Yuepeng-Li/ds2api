package claude

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"ds2api/internal/sse"
	streamengine "ds2api/internal/stream"
	"ds2api/internal/toolcall"
	"ds2api/internal/toolstream"
)

type claudeStreamRuntime struct {
	w        http.ResponseWriter
	rc       *http.ResponseController
	canFlush bool

	model           string
	toolNames       []string
	messages        []any
	toolsRaw        any
	promptTokenText string

	thinkingEnabled       bool
	searchEnabled         bool
	bufferToolContent     bool
	stripReferenceMarkers bool

	messageID string
	thinking  strings.Builder
	text      strings.Builder

	sieve                 toolstream.State
	rawText               strings.Builder
	rawThinking           strings.Builder
	toolDetectionThinking strings.Builder
	toolCallsDetected     bool

	nextBlockIndex     int
	thinkingBlockOpen  bool
	thinkingBlockIndex int
	textBlockOpen      bool
	textBlockIndex     int
	ended              bool
	upstreamErr        string
}

func newClaudeStreamRuntime(
	w http.ResponseWriter,
	rc *http.ResponseController,
	canFlush bool,
	model string,
	messages []any,
	thinkingEnabled bool,
	searchEnabled bool,
	stripReferenceMarkers bool,
	toolNames []string,
	toolsRaw any,
	promptTokenText string,
) *claudeStreamRuntime {
	return &claudeStreamRuntime{
		w:                     w,
		rc:                    rc,
		canFlush:              canFlush,
		model:                 model,
		messages:              messages,
		thinkingEnabled:       thinkingEnabled,
		searchEnabled:         searchEnabled,
		bufferToolContent:     len(toolNames) > 0,
		stripReferenceMarkers: stripReferenceMarkers,
		toolNames:             toolNames,
		toolsRaw:              toolsRaw,
		promptTokenText:       promptTokenText,
		messageID:             fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		thinkingBlockIndex:    -1,
		textBlockIndex:        -1,
	}
}

func (s *claudeStreamRuntime) onParsed(parsed sse.LineResult) streamengine.ParsedDecision {
	if !parsed.Parsed {
		return streamengine.ParsedDecision{}
	}
	if parsed.ErrorMessage != "" {
		s.upstreamErr = parsed.ErrorMessage
		return streamengine.ParsedDecision{Stop: true, StopReason: streamengine.StopReason("upstream_error")}
	}
	if parsed.Stop {
		return streamengine.ParsedDecision{Stop: true}
	}

	contentSeen := false
	for _, p := range parsed.ToolDetectionThinkingParts {
		trimmed := sse.TrimContinuationOverlapFromBuilder(&s.toolDetectionThinking, p.Text)
		if trimmed != "" {
			s.toolDetectionThinking.WriteString(trimmed)
		}
	}
	for _, p := range parsed.Parts {
		cleanedText := cleanVisibleOutput(p.Text, s.stripReferenceMarkers)
		if cleanedText == "" {
			continue
		}
		if p.Type != "thinking" && s.searchEnabled && sse.IsCitation(cleanedText) {
			continue
		}
		contentSeen = true

		if p.Type == "thinking" {
			s.rawThinking.WriteString(p.Text)
			if !s.thinkingEnabled {
				continue
			}
			if cleanedText == "" {
				continue
			}
			s.thinking.WriteString(cleanedText)
			s.closeTextBlock()
			if !s.thinkingBlockOpen {
				s.thinkingBlockIndex = s.nextBlockIndex
				s.nextBlockIndex++
				s.send("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": s.thinkingBlockIndex,
					"content_block": map[string]any{
						"type":     "thinking",
						"thinking": "",
					},
				})
				s.thinkingBlockOpen = true
			}
			s.send("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": s.thinkingBlockIndex,
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": cleanedText,
				},
			})
			continue
		}

		s.rawText.WriteString(p.Text)
		if cleanedText != "" {
			s.text.WriteString(cleanedText)
		}

		if !s.bufferToolContent {
			if cleanedText == "" {
				continue
			}
			s.closeThinkingBlock()
			if !s.textBlockOpen {
				s.textBlockIndex = s.nextBlockIndex
				s.nextBlockIndex++
				s.send("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": s.textBlockIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
				s.textBlockOpen = true
			}
			s.send("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": s.textBlockIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": cleanedText,
				},
			})
			continue
		}

		events := toolstream.ProcessChunk(&s.sieve, p.Text, s.toolNames)
		for _, evt := range events {
			if len(evt.ToolCalls) > 0 {
				s.closeTextBlock()
				s.toolCallsDetected = true
				normalized := toolcall.NormalizeParsedToolCallsForSchemas(evt.ToolCalls, s.toolsRaw)
				for _, tc := range normalized {
					idx := s.nextBlockIndex
					s.nextBlockIndex++
					s.sendToolUseBlock(idx, tc)
				}
				continue
			}
			if evt.Content == "" {
				continue
			}
			cleaned := cleanVisibleOutput(evt.Content, s.stripReferenceMarkers)
			if cleaned == "" || (s.searchEnabled && sse.IsCitation(cleaned)) {
				continue
			}
			s.closeThinkingBlock()
			if !s.textBlockOpen {
				s.textBlockIndex = s.nextBlockIndex
				s.nextBlockIndex++
				s.send("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": s.textBlockIndex,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
				s.textBlockOpen = true
			}
			s.send("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": s.textBlockIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": cleaned,
				},
			})
		}
	}

	return streamengine.ParsedDecision{ContentSeen: contentSeen}
}
