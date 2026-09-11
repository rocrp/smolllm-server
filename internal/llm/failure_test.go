package llm

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/stretchr/testify/require"
)

func failedAttempt(status int, disposition smolllm.Disposition, err error) smolllm.Attempt {
	return smolllm.Attempt{
		Provider:   "mock",
		Model:      "mock/marvin-7b",
		ModelName:  "marvin-7b",
		APIKeyHint: "sk-…cafe",
		Retry:      0,
		Usage:      smolllm.Usage{},
		Duration:   0,
		TTFT:       -1,
		Err: &smolllm.LegError{
			Provider:    "mock",
			Model:       "mock/marvin-7b",
			ModelName:   "marvin-7b",
			APIKeyHint:  "sk-…cafe",
			Retry:       0,
			StatusCode:  status,
			Disposition: disposition,
			Err:         err,
		},
	}
}

func TestFailureForMapsStopReasonToStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		msg        *smolllm.AssistantMessage
		wantStatus int
		wantKind   string
	}{
		{
			name:       "no result at all",
			msg:        nil,
			wantStatus: http.StatusBadGateway,
			wantKind:   "api_error",
		},
		{
			name: "client hung up",
			msg: &smolllm.AssistantMessage{
				StopReason:   smolllm.StopReasonAborted,
				ErrorMessage: "context canceled",
			},
			wantStatus: StatusClientClosedRequest,
			wantKind:   "api_error",
		},
		{
			name: "nothing was attempted",
			msg: &smolllm.AssistantMessage{
				StopReason:   smolllm.StopReasonError,
				ErrorMessage: "no models were attempted",
			},
			wantStatus: http.StatusBadRequest,
			wantKind:   "invalid_request_error",
		},
		{
			// A malformed request is wrong for every leg, so the chain aborts on
			// the first one. Reporting 502 would tell an agent to retry forever.
			name: "upstream rejected the request shape",
			msg: &smolllm.AssistantMessage{
				StopReason:   smolllm.StopReasonError,
				ErrorMessage: `leg "mock/marvin-7b": http error 400: bad tool schema`,
				Attempts: []smolllm.Attempt{
					failedAttempt(http.StatusBadRequest, smolllm.DispositionAbort,
						&smolllm.HTTPError{StatusCode: http.StatusBadRequest, Body: "bad tool schema"}),
				},
			},
			wantStatus: http.StatusBadRequest,
			wantKind:   "invalid_request_error",
		},
		{
			name: "payload too large exhausts the chain",
			msg: &smolllm.AssistantMessage{
				StopReason: smolllm.StopReasonError,
				Attempts: []smolllm.Attempt{
					failedAttempt(http.StatusRequestEntityTooLarge, smolllm.DispositionAdvance,
						&smolllm.HTTPError{StatusCode: http.StatusRequestEntityTooLarge, Body: "too big"}),
				},
			},
			wantStatus: http.StatusBadGateway,
			wantKind:   "api_error",
		},
		{
			// 401 is leg-local: the chain advanced and ran out of candidates, so
			// the caller's request was never the problem.
			name: "every leg exhausted its credentials",
			msg: &smolllm.AssistantMessage{
				StopReason: smolllm.StopReasonError,
				Attempts: []smolllm.Attempt{
					failedAttempt(http.StatusUnauthorized, smolllm.DispositionAdvance,
						&smolllm.HTTPError{StatusCode: http.StatusUnauthorized, Body: "denied"}),
					failedAttempt(http.StatusTooManyRequests, smolllm.DispositionAdvance,
						&smolllm.HTTPError{StatusCode: http.StatusTooManyRequests, Body: "slow down"}),
				},
			},
			wantStatus: http.StatusBadGateway,
			wantKind:   "api_error",
		},
		{
			name: "whole-call deadline expired",
			msg: &smolllm.AssistantMessage{
				StopReason: smolllm.StopReasonError,
				Attempts: []smolllm.Attempt{
					failedAttempt(0, smolllm.DispositionAbort, context.DeadlineExceeded),
				},
			},
			wantStatus: http.StatusGatewayTimeout,
			wantKind:   "api_error",
		},
		{
			name: "connection failure advances then exhausts",
			msg: &smolllm.AssistantMessage{
				StopReason: smolllm.StopReasonError,
				Attempts: []smolllm.Attempt{
					failedAttempt(0, smolllm.DispositionAdvance, errors.New("connection refused")),
				},
			},
			wantStatus: http.StatusBadGateway,
			wantKind:   "api_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			failure := FailureFor(tt.msg)
			require.Equal(t, tt.wantStatus, failure.Status)
			require.Equal(t, tt.wantKind, failure.Kind)
			require.NotEmpty(t, failure.Message, "a failure must always say something")
		})
	}
}

// The chain records every attempt, successful legs included, so the status must
// come from the leg that actually ended the call.
func TestFailureForIgnoresSucceededAttempts(t *testing.T) {
	t.Parallel()

	msg := &smolllm.AssistantMessage{
		StopReason: smolllm.StopReasonError,
		Attempts: []smolllm.Attempt{
			{Provider: "mock", Model: "mock/a", ModelName: "a"},
			failedAttempt(http.StatusBadRequest, smolllm.DispositionAbort,
				&smolllm.HTTPError{StatusCode: http.StatusBadRequest, Body: "nope"}),
		},
	}
	require.Equal(t, http.StatusBadRequest, FailureFor(msg).Status)
}

func TestFailureForAlwaysCarriesAMessage(t *testing.T) {
	t.Parallel()

	failure := FailureFor(&smolllm.AssistantMessage{StopReason: smolllm.StopReasonError})
	require.Contains(t, failure.Message, string(smolllm.StopReasonError))
}

func TestUsageFromAddsCachedTokensBackIntoPrompt(t *testing.T) {
	t.Parallel()

	// smolllm-go reports Input without the cache reads; OpenAI clients read
	// prompt_tokens as the whole prompt and the cached share as a detail.
	usage := UsageFrom(smolllm.Usage{
		Input:     100,
		Output:    40,
		CacheRead: 900,
		Reasoning: 25,
		Total:     1040,
		Estimated: false,
	})

	require.Equal(t, 1000, usage.PromptTokens)
	require.Equal(t, 40, usage.CompletionTokens)
	require.Equal(t, 1040, usage.TotalTokens)
	require.NotNil(t, usage.PromptTokensDetails)
	require.Equal(t, 900, usage.PromptTokensDetails.CachedTokens)
	require.NotNil(t, usage.CompletionTokensDetails)
	require.Equal(t, 25, usage.CompletionTokensDetails.ReasoningTokens)
}

func TestUsageFromOmitsDetailsProvidersDidNotReport(t *testing.T) {
	t.Parallel()

	usage := UsageFrom(smolllm.Usage{Input: 3, Output: 1, Total: 4})
	require.Equal(t, 3, usage.PromptTokens)
	require.Nil(t, usage.PromptTokensDetails)
	require.Nil(t, usage.CompletionTokensDetails)
}
