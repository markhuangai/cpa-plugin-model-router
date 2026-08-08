package main

import (
	"context"
	"errors"
	"testing"
)

func TestEligibleRouteFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "rate limit", err: statusError{status: 429, message: "rate limited"}, want: true},
		{name: "provider server error", err: statusError{status: 503, message: "unavailable"}, want: true},
		{name: "bad request", err: statusError{status: 400, message: "invalid prompt"}, want: false},
		{name: "persisted response miss", err: statusError{status: 404, message: "items are not persisted when store is false"}, want: false},
		{name: "model missing", err: statusError{status: 404, message: "model not found"}, want: true},
		{name: "canceled", err: context.Canceled, want: false},
		{name: "recognized transport", err: errors.New("connection reset by peer"), want: true},
		{name: "ambiguous error", err: errors.New("execution failed"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := eligibleRouteFailure(test.err); got != test.want {
				t.Fatalf("eligibleRouteFailure(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
