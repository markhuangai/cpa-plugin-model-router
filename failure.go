package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var statusPattern = regexp.MustCompile(`(?i)(?:status|http status|status_code)[^0-9]*(\d{3})`)

type statusError struct {
	status  int
	code    string
	message string
}

func (err statusError) Error() string {
	if err.message != "" {
		return err.message
	}
	if err.status > 0 {
		return fmt.Sprintf("model execution failed with status %d", err.status)
	}
	return "model execution failed"
}

func (err statusError) StatusCode() int   { return err.status }
func (err statusError) ErrorCode() string { return err.code }

func newRouteError(status int, code, alias, message, detail string) error {
	errorBody := map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"alias":   strings.TrimSpace(alias),
		},
	}
	if strings.TrimSpace(detail) != "" {
		errorBody["error"].(map[string]any)["detail"] = strings.TrimSpace(detail)
	}
	raw, err := json.Marshal(errorBody)
	if err != nil {
		raw = []byte(message)
	}
	return statusError{status: status, code: code, message: string(raw)}
}

func statusFromError(err error) int {
	if err == nil {
		return 0
	}
	var carrier interface{ StatusCode() int }
	if errors.As(err, &carrier) && carrier.StatusCode() > 0 {
		return carrier.StatusCode()
	}
	match := statusPattern.FindStringSubmatch(err.Error())
	if len(match) != 2 {
		return 0
	}
	status, parseErr := strconv.Atoi(match[1])
	if parseErr != nil || status < 100 || status > 599 {
		return 0
	}
	return status
}

func codeFromError(err error, fallback string) string {
	var carrier interface{ ErrorCode() string }
	if errors.As(err, &carrier) && strings.TrimSpace(carrier.ErrorCode()) != "" {
		return carrier.ErrorCode()
	}
	return fallback
}

// fallbackPolicy decides whether a failed attempt moves on to the next target.
// The status sets come from plugin configuration, so they can be changed without
// rebuilding the library.
type fallbackPolicy struct {
	onStatus   map[int]struct{}
	noFallback map[int]struct{}
}

func newFallbackPolicy(fallbackOnStatus, noFallbackOnStatus []int) fallbackPolicy {
	policy := fallbackPolicy{
		onStatus:   make(map[int]struct{}, len(fallbackOnStatus)),
		noFallback: make(map[int]struct{}, len(noFallbackOnStatus)),
	}
	for _, status := range fallbackOnStatus {
		policy.onStatus[status] = struct{}{}
	}
	for _, status := range noFallbackOnStatus {
		policy.noFallback[status] = struct{}{}
	}
	return policy
}

// shouldFallback reports whether a non-2xx status moves the request to the next
// target. no_fallback_on_status takes precedence over fallback_on_status. A 5xx
// that appears in neither list still falls back, because it reports a fault at
// the target rather than a rejected request.
func (p fallbackPolicy) shouldFallback(status int) bool {
	if _, excluded := p.noFallback[status]; excluded {
		return false
	}
	if _, listed := p.onStatus[status]; listed {
		return true
	}
	return status >= http.StatusInternalServerError
}

func eligibleRouteFailure(err error, policy fallbackPolicy) bool {
	if err == nil || terminalRequestError(err) {
		return false
	}
	if status := statusFromError(err); status > 0 {
		if persistedResponseMiss(status, err.Error()) {
			return false
		}
		return policy.shouldFallback(status)
	}
	return recognizableTransientError(err)
}

// persistedResponseMiss reports the 404 that describes the request rather than
// the target. An upstream asked not to persist reports a miss that every other
// target reproduces, so it must not fail over or cool the route.
func persistedResponseMiss(status int, message string) bool {
	if status != http.StatusNotFound {
		return false
	}
	message = strings.ToLower(message)
	return strings.Contains(message, "items are not persisted") || (strings.Contains(message, "store") && strings.Contains(message, "false"))
}

func terminalRequestError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, token := range []string{"context canceled", "context deadline exceeded", "client disconnected", "request canceled", "request cancelled"} {
		if strings.Contains(message, token) {
			return true
		}
	}
	return false
}

func recognizableTransientError(err error) bool {
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, token := range []string{
		"rate limit", "ratelimit", "too many requests", "quota", "exceed your account",
		"auth_not_found", "auth_unavailable", "model_cooldown", "no auth available", "no active auth", "no available auth",
		"no active account", "no available account", "account disabled", "auth disabled", "credential disabled", "credentials disabled",
		"unknown provider", "no provider for model", "provider unavailable", "model unavailable",
		"timed out", "timeout", "connection reset", "connection refused", "connection aborted", "broken pipe", "no such host", "network is unreachable", "temporary failure", "eof",
	} {
		if strings.Contains(message, token) {
			return true
		}
	}
	return false
}
