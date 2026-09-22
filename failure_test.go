package main

import (
	"context"
	"errors"
	"testing"
)

func TestEligibleRouteFailure(t *testing.T) {
	policy := newFallbackPolicy(defaultFallbackOnStatus, nil)
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "rate limit", err: statusError{status: 429, message: "rate limited"}, want: true},
		{name: "provider server error", err: statusError{status: 503, message: "unavailable"}, want: true},
		{name: "bad request", err: statusError{status: 400, message: "invalid prompt"}, want: true},
		{name: "unprocessable", err: statusError{status: 422, message: "invalid payload"}, want: true},
		{name: "conflict", err: statusError{status: 409, message: "conflict"}, want: true},
		{name: "payment required", err: statusError{status: 402, message: "package_quota_exhausted"}, want: true},
		{name: "persisted response miss", err: statusError{status: 404, message: "items are not persisted when store is false"}, want: true},
		{name: "model missing", err: statusError{status: 404, message: "model not found"}, want: true},
		{name: "unlisted client error", err: statusError{status: 413, message: "payload too large"}, want: false},
		{name: "unlisted server error", err: statusError{status: 507, message: "insufficient storage"}, want: true},
		{name: "cloudflare timeout", err: statusError{status: 524, message: "a timeout occurred"}, want: true},
		{name: "canceled", err: context.Canceled, want: false},
		{name: "recognized transport", err: errors.New("connection reset by peer"), want: true},
		{name: "tls handshake timeout", err: errors.New("net/http: TLS handshake timeout"), want: true},
		{name: "ambiguous error", err: errors.New("execution failed"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := eligibleRouteFailure(test.err, policy); got != test.want {
				t.Fatalf("eligibleRouteFailure(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestFallbackPolicyNoFallbackWins(t *testing.T) {
	policy := newFallbackPolicy([]int{400, 429}, []int{429})
	if !policy.shouldFallback(400) {
		t.Fatal("400 should fall back when listed in fallback_on_status")
	}
	if policy.shouldFallback(429) {
		t.Fatal("no_fallback_on_status must win over fallback_on_status")
	}
	if policy.shouldFallback(401) {
		t.Fatal("an unlisted 4xx must not fall back")
	}
	if !policy.shouldFallback(500) {
		t.Fatal("every remaining 5xx must fall back")
	}
}
