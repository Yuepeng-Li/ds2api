package claude

import (
	"ds2api/internal/sse"
	"ds2api/internal/toolcall"
	"ds2api/internal/toolstream"
	"encoding/json"
	"fmt"
	"time"

	streamengine "ds2api/internal/stream"
	"ds2api/internal/util"
)

func (s *claudeStreamRuntime) closeThinkingBlock() {
	if !s.thinkingBlockOpen {
		return
	}
	s.send("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": s.thinkingBlockIndex,
	})
	s.thinkingBlockOpen = false
	s.thinkingBlockIndex = -1
}

func (s *claudeStreamRuntime) closeTextBlock() {
	if !s.textBlockOpen {
		return
	}
	s.send("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": s.textBlockIndex,
	})
	s.textBlockOpen = false
	s.textBlockIndex = -1
}

func (s *claudeStreamRuntime) sendToolUseBlock(idx int, tc toolcall.ParsedToolCall) {
	s.send("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    fmt.Sprintf("toolu_%d_%d", time.Now().Unix(), idx),
			"name":  tc.Name,
			"input": map[string]any{},
		},
	})
	inputBytes, _ := json.Marshal(tc.Input)
	s.send("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": string(inputBytes),
		},
	})
	s.send("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

func (s *claudeStreamRuntime) finalize(stopReason string) {
	if s.ended {
		return
	}
	s.ended = true

	s.closeThinkingBlock()

	if s.bufferToolContent {
		for _, evt := range toolstream.Flush(&s.sieve, s.toolNames) {
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
			if evt.Content != "" {
				cleaned := cleanVisibleOutput(evt.Content, s.stripReferenceMarkers)
				if cleaned == "" || (s.searchEnabled && sse.IsCitation(cleaned)) {
					continue
				}
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
	}

	s.closeTextBlock()

	finalThinking := s.thinking.String()
	finalText := cleanVisibleOutput(s.text.String(), s.stripReferenceMarkers)

	if s.bufferToolContent && !s.toolCallsDetected {
		detected := toolcall.ParseStandaloneToolCallsDetailed(s.rawText.String(), s.toolNames)
		if len(detected.Calls) == 0 {
			detected = toolcall.ParseStandaloneToolCallsDetailed(s.rawThinking.String(), s.toolNames)
		}
		if len(detected.Calls) > 0 {
			normalized := toolcall.NormalizeParsedToolCallsForSchemas(detected.Calls, s.toolsRaw)
			stopReason = "tool_use"
			for _, tc := range normalized {
				idx := s.nextBlockIndex
				s.nextBlockIndex++
				s.sendToolUseBlock(idx, tc)
			}
		} else if finalText != "" && !s.textBlockOpen {
			idx := s.nextBlockIndex
			s.nextBlockIndex++
			s.send("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			s.send("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]any{
					"type": "text_delta",
					"text": finalText,
				},
			})
			s.send("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": idx,
			})
		}
	}

	if s.toolCallsDetected {
		stopReason = "tool_use"
	}

	outputTokens := util.CountOutputTokens(finalThinking, s.model) + util.CountOutputTokens(finalText, s.model)
	s.send("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": outputTokens,
		},
	})
	s.send("message_stop", map[string]any{"type": "message_stop"})
}

func (s *claudeStreamRuntime) onFinalize(reason streamengine.StopReason, scannerErr error) {
	if string(reason) == "upstream_error" {
		s.sendError(s.upstreamErr)
		return
	}
	if scannerErr != nil {
		s.sendError(scannerErr.Error())
		return
	}
	s.finalize("end_turn")
}
