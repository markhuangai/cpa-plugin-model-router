package main

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type executionBody struct {
	body             []byte
	entryProtocol    string
	responseProtocol string
}

func bodyForExecution(request pluginapi.ExecutorRequest) executionBody {
	source := normalizeProtocol(request.SourceFormat)
	format := normalizeProtocol(request.Format)
	if len(request.OriginalRequest) > 0 {
		entry := firstNonEmpty(source, inferProtocol(request.OriginalRequest), format, "openai")
		return executionBody{body: bytes.Clone(request.OriginalRequest), entryProtocol: entry, responseProtocol: firstNonEmpty(source, entry)}
	}
	if len(request.Payload) > 0 {
		entry := firstNonEmpty(format, inferProtocol(request.Payload), source, "openai")
		return executionBody{body: bytes.Clone(request.Payload), entryProtocol: entry, responseProtocol: firstNonEmpty(source, entry)}
	}
	entry := firstNonEmpty(source, format, "openai")
	return executionBody{entryProtocol: entry, responseProtocol: firstNonEmpty(source, entry)}
}

func requestBodyForTarget(body []byte, model string) []byte {
	if len(bytes.TrimSpace(body)) == 0 || strings.TrimSpace(model) == "" || !gjson.ValidBytes(body) || !gjson.GetBytes(body, "model").Exists() {
		return bytes.Clone(body)
	}
	updated, err := sjson.SetBytes(body, "model", strings.TrimSpace(model))
	if err != nil {
		return bytes.Clone(body)
	}
	return updated
}

func inferProtocol(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	if gjson.GetBytes(body, "input").Exists() || gjson.GetBytes(body, "instructions").Exists() {
		return "openai-response"
	}
	if gjson.GetBytes(body, "system").Exists() {
		return "claude"
	}
	if gjson.GetBytes(body, "messages").Exists() {
		return "openai"
	}
	if gjson.GetBytes(body, "contents").Exists() {
		return "gemini"
	}
	return ""
}

func normalizeProtocol(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "anthropic":
		return "claude"
	case "responses", "openai-responses", "openai_responses":
		return "openai-response"
	case "chat-completions", "chat_completions", "openai-chat-completions", "openai_chat_completions":
		return "openai"
	case "gemini-cli":
		return "interactions"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func sanitizeNestedHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	out := make(http.Header)
	for key, values := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower == "" || sensitiveNestedHeader(lower) {
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

func sensitiveNestedHeader(lower string) bool {
	switch lower {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key", "apikey", "x-goog-api-key", "host", "content-length":
		return true
	}
	return strings.Contains(lower, "api-key") || strings.Contains(lower, "apikey") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "credential")
}

func cloneHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	out := make(http.Header, len(headers))
	for key, values := range headers {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func cloneValues(values url.Values) url.Values {
	if values == nil {
		return nil
	}
	out := make(url.Values, len(values))
	for key, items := range values {
		out[key] = append([]string(nil), items...)
	}
	return out
}
