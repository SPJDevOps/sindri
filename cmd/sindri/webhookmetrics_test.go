package main

import (
	"errors"
	"testing"

	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/worker"
)

func TestWebhookResult_ClassifiesAnAttempt(t *testing.T) {
	tests := []struct {
		name string
		job  queue.Job
		err  error
		want string
	}{
		{name: "NoErrorIsDelivered", job: queue.Job{Attempts: 1, MaxAttempts: 3}, want: "delivered"},
		{name: "ErrorWithAttemptsLeftIsRetried", job: queue.Job{Attempts: 1, MaxAttempts: 3}, err: errors.New("503"), want: "retried"},
		{name: "ErrorOnTheLastAttemptIsFailed", job: queue.Job{Attempts: 3, MaxAttempts: 3}, err: errors.New("503"), want: "failed"},
		{name: "PermanentErrorIsFailedAtOnce", job: queue.Job{Attempts: 1, MaxAttempts: 3}, err: worker.Permanent(errors.New("404")), want: "failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhookResult(tc.job, tc.err); got != tc.want {
				t.Errorf("webhookResult = %s, want %s", got, tc.want)
			}
		})
	}
}
