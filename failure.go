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

func eligibleRouteFailure(err error) bool {
	if err == nil || terminalRequestError(err) {
		return false
	}
	status := statusFromError(err)
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return false
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	case http.StatusNotFound:
		message := strings.ToLower(err.Error())
		return !strings.Contains(message, "items are not persisted") && !(strings.Contains(message, "store") && strings.Contains(message, "false"))
	default:
		if status >= http.StatusInternalServerError {
			return true
		}
		if status > 0 {
			return false
		}
	}
	return recognizableTransientError(err)
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
		"timed out", "connection reset", "connection refused", "connection aborted", "broken pipe", "no such host", "network is unreachable", "temporary failure", "eof",
	} {
		if strings.Contains(message, token) {
			return true
		}
	}
	return false
}
