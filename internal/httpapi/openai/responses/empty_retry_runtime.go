package responses

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"ds2api/internal/auth"
	"ds2api/internal/config"
	dsprotocol "ds2api/internal/deepseek/protocol"
	openaifmt "ds2api/internal/format/openai"
	"ds2api/internal/promptcompat"
	"ds2api/internal/sse"
	streamengine "ds2api/internal/stream"
	"ds2api/internal/toolcall"
)

type responsesNonStreamResult struct {
	rawThinking           string
	rawText               string
	thinking              string
	toolDetectionThinking string
	text                  string
	contentFilter         bool
	parsed                toolcall.ToolCallParseResult
	body                  map[string]any
	responseMessageID     int
}

func (h *Handler) handleResponsesNonStreamWithRetry(w http.ResponseWriter, ctx context.Context, a *auth.RequestAuth, resp *http.Response, payload map[string]any, pow, owner, responseID, model, finalPrompt string, refFileTokens int, thinkingEnabled, searchEnabled bool, toolNames []string, toolsRaw any, toolChoice promptcompat.ToolChoicePolicy, traceID string) {
	attempts := 0
	currentResp := resp
	usagePrompt := finalPrompt
	accumulatedThinking := ""
	accumulatedRawThinking := ""
	accumulatedToolDetectionThinking := ""
	for {
		result, ok := h.collectResponsesNonStreamAttempt(w, currentResp, responseID, model, usagePrompt, thinkingEnabled, searchEnabled, toolNames, toolsRaw)
		if !ok {
			return
		}
		accumulatedThinking += sse.TrimContinuationOverlap(accumulatedThinking, result.thinking)
		accumulatedRawThinking += sse.TrimContinuationOverlap(accumulatedRawThinking, result.rawThinking)
		accumulatedToolDetectionThinking += sse.TrimContinuationOverlap(accumulatedToolDetectionThinking, result.toolDetectionThinking)
		result.thinking = accumulatedThinking
		result.rawThinking = accumulatedRawThinking
		result.toolDetectionThinking = accumulatedToolDetectionThinking
		result.parsed = detectAssistantToolCalls(result.rawText, result.text, result.rawThinking, result.toolDetectionThinking, toolNames)
		result.body = openaifmt.BuildResponseObjectWithToolCalls(responseID, model, usagePrompt, result.thinking, result.text, result.parsed.Calls, toolsRaw)
		if refFileTokens > 0 {
			addRefFileTokensToUsage(result.body, refFileTokens)
		}

		if !shouldRetryResponsesNonStream(result, attempts) {
			h.finishResponsesNonStreamResult(w, result, attempts, owner, responseID, toolChoice, traceID)
			return
		}

		attempts++
		config.Logger.Info("[openai_empty_retry] attempting synthetic retry", "surface", "responses", "stream", false, "retry_attempt", attempts, "parent_message_id", result.responseMessageID)
		retryPow, powErr := h.DS.GetPow(ctx, a, 3)
		if powErr != nil {
			config.Logger.Warn("[openai_empty_retry] retry PoW fetch failed, falling back to original PoW", "surface", "responses", "stream", false, "retry_attempt", attempts, "error", powErr)
			retryPow = pow
		}
		nextResp, err := h.DS.CallCompletion(ctx, a, clonePayloadForEmptyOutputRetry(payload, result.responseMessageID), retryPow, 3)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "Failed to get completion.")
			config.Logger.Warn("[openai_empty_retry] retry request failed", "surface", "responses", "stream", false, "retry_attempt", attempts, "error", err)
			return
		}
		usagePrompt = usagePromptWithEmptyOutputRetry(usagePrompt, attempts)
		currentResp = nextResp
	}
}

func (h *Handler) collectResponsesNonStreamAttempt(w http.ResponseWriter, resp *http.Response, responseID, model, usagePrompt string, thinkingEnabled, searchEnabled bool, toolNames []string, toolsRaw any) (responsesNonStreamResult, bool) {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		writeOpenAIError(w, resp.StatusCode, strings.TrimSpace(string(body)))
		return responsesNonStreamResult{}, false
	}
	result := sse.CollectStream(resp, thinkingEnabled, false)
	stripReferenceMarkers := h.compatStripReferenceMarkers()
	sanitizedThinking := cleanVisibleOutput(result.Thinking, stripReferenceMarkers)
	sanitizedText := cleanVisibleOutput(result.Text, stripReferenceMarkers)
	if searchEnabled {
		sanitizedText = replaceCitationMarkersWithLinks(sanitizedText, result.CitationLinks)
	}
	textParsed := detectAssistantToolCalls(result.Text, sanitizedText, result.Thinking, result.ToolDetectionThinking, toolNames)
	responseObj := openaifmt.BuildResponseObjectWithToolCalls(responseID, model, usagePrompt, sanitizedThinking, sanitizedText, textParsed.Calls, toolsRaw)
	return responsesNonStreamResult{
		rawThinking:           result.Thinking,
		rawText:               result.Text,
		thinking:              sanitizedThinking,
		toolDetectionThinking: result.ToolDetectionThinking,
		text:                  sanitizedText,
		contentFilter:         result.ContentFilter,
		parsed:                textParsed,
		body:                  responseObj,
		responseMessageID:     result.ResponseMessageID,
	}, true
}

func (h *Handler) finishResponsesNonStreamResult(w http.ResponseWriter, result responsesNonStreamResult, attempts int, owner, responseID string, toolChoice promptcompat.ToolChoicePolicy, traceID string) {
	if len(result.parsed.Calls) == 0 && writeUpstreamEmptyOutputError(w, result.text, result.thinking, result.contentFilter) {
		config.Logger.Info("[openai_empty_retry] terminal empty output", "surface", "responses", "stream", false, "retry_attempts", attempts, "success_source", "none", "content_filter", result.contentFilter)
		return
	}
	logResponsesToolPolicyRejection(traceID, toolChoice, result.parsed, "text")
	if toolChoice.IsRequired() && len(result.parsed.Calls) == 0 {
		writeOpenAIErrorWithCode(w, http.StatusUnprocessableEntity, "tool_choice requires at least one valid tool call.", "tool_choice_violation")
		return
	}
	h.getResponseStore().put(owner, responseID, result.body)
	writeJSON(w, http.StatusOK, result.body)
	source := "first_attempt"
	if attempts > 0 {
		source = "synthetic_retry"
	}
	config.Logger.Info("[openai_empty_retry] completed", "surface", "responses", "stream", false, "retry_attempts", attempts, "success_source", source)
}

func shouldRetryResponsesNonStream(result responsesNonStreamResult, attempts int) bool {
	return emptyOutputRetryEnabled() &&
		attempts < emptyOutputRetryMaxAttempts() &&
		!result.contentFilter &&
		len(result.parsed.Calls) == 0 &&
		strings.TrimSpace(result.text) == "" &&
		strings.TrimSpace(result.thinking) == ""
}

func (h *Handler) handleResponsesStreamWithRetry(w http.ResponseWriter, r *http.Request, a *auth.RequestAuth, resp *http.Response, payload map[string]any, pow, owner, responseID, model, finalPrompt string, refFileTokens int, thinkingEnabled, searchEnabled bool, toolNames []string, toolsRaw any, toolChoice promptcompat.ToolChoicePolicy, traceID string) {
	streamRuntime, initialType, ok := h.prepareResponsesStreamRuntime(w, resp, owner, responseID, model, finalPrompt, refFileTokens, thinkingEnabled, searchEnabled, toolNames, toolsRaw, toolChoice, traceID)
	if !ok {
		return
	}
	attempts := 0
	currentResp := resp
	for {
		terminalWritten, retryable := h.consumeResponsesStreamAttempt(r, currentResp, streamRuntime, initialType, thinkingEnabled, attempts < emptyOutputRetryMaxAttempts())
		if terminalWritten {
			logResponsesStreamTerminal(streamRuntime, attempts)
			return
		}
		if !retryable || !emptyOutputRetryEnabled() || attempts >= emptyOutputRetryMaxAttempts() {
			streamRuntime.finalize("stop", false)
			config.Logger.Info("[openai_empty_retry] terminal empty output", "surface", "responses", "stream", true, "retry_attempts", attempts, "success_source", "none", "error_code", streamRuntime.finalErrorCode)
			return
		}
		attempts++
		config.Logger.Info("[openai_empty_retry] attempting synthetic retry", "surface", "responses", "stream", true, "retry_attempt", attempts, "parent_message_id", streamRuntime.responseMessageID)
		retryPow, powErr := h.DS.GetPow(r.Context(), a, 3)
		if powErr != nil {
			config.Logger.Warn("[openai_empty_retry] retry PoW fetch failed, falling back to original PoW", "surface", "responses", "stream", true, "retry_attempt", attempts, "error", powErr)
			retryPow = pow
		}
		nextResp, err := h.DS.CallCompletion(r.Context(), a, clonePayloadForEmptyOutputRetry(payload, streamRuntime.responseMessageID), retryPow, 3)
		if err != nil {
			streamRuntime.failResponse(http.StatusInternalServerError, "Failed to get completion.", "error")
			config.Logger.Warn("[openai_empty_retry] retry request failed", "surface", "responses", "stream", true, "retry_attempt", attempts, "error", err)
			return
		}
		if nextResp.StatusCode != http.StatusOK {
			defer func() { _ = nextResp.Body.Close() }()
			body, _ := io.ReadAll(nextResp.Body)
			streamRuntime.failResponse(nextResp.StatusCode, strings.TrimSpace(string(body)), "error")
			return
		}
		streamRuntime.finalPrompt = usagePromptWithEmptyOutputRetry(finalPrompt, attempts)
		currentResp = nextResp
	}
}

func (h *Handler) prepareResponsesStreamRuntime(w http.ResponseWriter, resp *http.Response, owner, responseID, model, finalPrompt string, refFileTokens int, thinkingEnabled, searchEnabled bool, toolNames []string, toolsRaw any, toolChoice promptcompat.ToolChoicePolicy, traceID string) (*responsesStreamRuntime, string, bool) {
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		writeOpenAIError(w, resp.StatusCode, strings.TrimSpace(string(body)))
		return nil, "", false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	_, canFlush := w.(http.Flusher)
	initialType := "text"
	if thinkingEnabled {
		initialType = "thinking"
	}
	streamRuntime := newResponsesStreamRuntime(
		w, rc, canFlush, responseID, model, finalPrompt, thinkingEnabled, searchEnabled,
		h.compatStripReferenceMarkers(), toolNames, toolsRaw, len(toolNames) > 0,
		h.toolcallFeatureMatchEnabled() && h.toolcallEarlyEmitHighConfidence(),
		toolChoice, traceID, func(obj map[string]any) {
			h.getResponseStore().put(owner, responseID, obj)
		},
	)
	streamRuntime.refFileTokens = refFileTokens
	streamRuntime.sendCreated()
	return streamRuntime, initialType, true
}

func (h *Handler) consumeResponsesStreamAttempt(r *http.Request, resp *http.Response, streamRuntime *responsesStreamRuntime, initialType string, thinkingEnabled bool, allowDeferEmpty bool) (bool, bool) {
	defer func() { _ = resp.Body.Close() }()
	finalReason := "stop"
	streamengine.ConsumeSSE(streamengine.ConsumeConfig{
		Context:             r.Context(),
		Body:                resp.Body,
		ThinkingEnabled:     thinkingEnabled,
		InitialType:         initialType,
		KeepAliveInterval:   time.Duration(dsprotocol.KeepAliveTimeout) * time.Second,
		IdleTimeout:         time.Duration(dsprotocol.StreamIdleTimeout) * time.Second,
		MaxKeepAliveNoInput: dsprotocol.MaxKeepaliveCount,
	}, streamengine.ConsumeHooks{
		OnParsed: streamRuntime.onParsed,
		OnFinalize: func(reason streamengine.StopReason, _ error) {
			if string(reason) == "content_filter" {
				finalReason = "content_filter"
			}
		},
		OnContextDone: func() {
			streamRuntime.markContextCancelled()
		},
	})
	if streamRuntime.finalErrorCode == string(streamengine.StopReasonContextCancelled) {
		return true, false
	}
	terminalWritten := streamRuntime.finalize(finalReason, allowDeferEmpty && finalReason != "content_filter")
	if terminalWritten {
		return true, false
	}
	return false, true
}

func logResponsesStreamTerminal(streamRuntime *responsesStreamRuntime, attempts int) {
	source := "first_attempt"
	if attempts > 0 {
		source = "synthetic_retry"
	}
	if streamRuntime.finalErrorCode == string(streamengine.StopReasonContextCancelled) {
		config.Logger.Info("[openai_empty_retry] terminal cancelled", "surface", "responses", "stream", true, "retry_attempts", attempts, "error_code", streamRuntime.finalErrorCode)
		return
	}
	if streamRuntime.failed {
		config.Logger.Info("[openai_empty_retry] terminal empty output", "surface", "responses", "stream", true, "retry_attempts", attempts, "success_source", "none", "error_code", streamRuntime.finalErrorCode)
		return
	}
	config.Logger.Info("[openai_empty_retry] completed", "surface", "responses", "stream", true, "retry_attempts", attempts, "success_source", source)
}
