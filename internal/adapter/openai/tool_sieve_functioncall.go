package openai

import "strings"

func findQuotedFunctionCallKeyStart(s string) int {
	lower := strings.ToLower(s)
	quotedIdx := findFunctionCallKeyStart(lower, `"functioncall"`)
	bareIdx := findFunctionCallKeyStart(lower, "functioncall")

	// Prefer the quoted JSON key whenever we have a structural match.
	// Bare-key detection is only for loose payloads where the quoted form
	// is absent.
	if quotedIdx >= 0 {
		return quotedIdx
	}
	return bareIdx
}

func findFunctionCallKeyStart(lower, key string) int {
	for from := 0; from < len(lower); {
		rel := strings.Index(lower[from:], key)
		if rel < 0 {
			return -1
		}
		idx := from + rel
		if isInsideJSONString(lower, idx) {
			from = idx + 1
			continue
		}
		if !hasJSONObjectContextPrefix(lower[:idx]) {
			from = idx + 1
			continue
		}
		if !hasJSONKeyBoundary(lower, idx, len(key)) {
			from = idx + 1
			continue
		}
		j := idx + len(key)
		for j < len(lower) && (lower[j] == ' ' || lower[j] == '\t' || lower[j] == '\r' || lower[j] == '\n') {
			j++
		}
		if j < len(lower) && lower[j] == ':' {
			k := j + 1
			for k < len(lower) && (lower[k] == ' ' || lower[k] == '\t' || lower[k] == '\r' || lower[k] == '\n') {
				k++
			}
			if k < len(lower) && lower[k] != '{' {
				from = idx + 1
				continue
			}
			return idx
		}
		from = idx + 1
	}
	return -1
}

func isInsideJSONString(s string, idx int) bool {
	inString := false
	escaped := false
	for i := 0; i < idx; i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
		}
	}
	return inString
}

func hasJSONObjectContextPrefix(prefix string) bool {
	return strings.LastIndex(prefix, "{") >= 0
}

func hasJSONKeyBoundary(s string, idx, keyLen int) bool {
	if idx > 0 {
		prev := s[idx-1]
		if isLowerAlphaNumeric(prev) {
			return false
		}
	}
	if end := idx + keyLen; end < len(s) {
		next := s[end]
		if isLowerAlphaNumeric(next) {
			return false
		}
	}
	return true
}

func isLowerAlphaNumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}
