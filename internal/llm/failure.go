package llm

import (
	"context"
	"errors"
	"net/http"

	"github.com/rocry/smolllm-go/smolllm"
)

// StatusClientClosedRequest is nginx's code for a client that hung up mid-call.
// Go has no constant for it, and no standard code says the same thing.
const StatusClientClosedRequest = 499

// Failure is the OpenAI-style error envelope for a turn that produced no answer.
type Failure struct {
	Status  int
	Code    string
	Kind    string
	Message string
}

// FailureFor maps a failed turn onto the response the client sees.
//
// smolllm-go v0.3 never reports an operational failure as a Go error: the turn
// comes back with a StopReason and an ErrorMessage naming every failed leg. It
// also classifies each leg, and that classification is the only honest source
// for a status code. A malformed request aborts the whole chain, so reporting it
// as 502 would tell an agent loop to retry something that can never succeed.
func FailureFor(msg *smolllm.AssistantMessage) Failure {
	if msg == nil {
		return Failure{
			Status:  http.StatusBadGateway,
			Code:    "upstream_error",
			Kind:    "api_error",
			Message: "upstream call produced no result",
		}
	}

	message := msg.ErrorMessage
	if message == "" {
		message = "upstream call failed with reason " + string(msg.StopReason)
	}

	if msg.StopReason == smolllm.StopReasonAborted {
		return Failure{
			Status:  StatusClientClosedRequest,
			Code:    "client_closed_request",
			Kind:    "api_error",
			Message: message,
		}
	}

	leg := decisiveLeg(msg.Attempts)
	switch {
	case leg == nil:
		// Nothing was ever attempted, so the chain rejected the call itself: an
		// unusable request or a model spec that resolved to no candidate.
		return Failure{
			Status:  http.StatusBadRequest,
			Code:    "invalid_request",
			Kind:    "invalid_request_error",
			Message: message,
		}
	case errors.Is(leg, context.DeadlineExceeded):
		return Failure{
			Status:  http.StatusGatewayTimeout,
			Code:    "upstream_timeout",
			Kind:    "api_error",
			Message: message,
		}
	case leg.Disposition == smolllm.DispositionAbort && isClientError(leg.StatusCode):
		// The request shape is wrong for every provider, so the caller has to
		// change it. Handing back the upstream's own status says exactly that.
		return Failure{
			Status:  leg.StatusCode,
			Code:    "invalid_request",
			Kind:    "invalid_request_error",
			Message: message,
		}
	default:
		return Failure{
			Status:  http.StatusBadGateway,
			Code:    "upstream_error",
			Kind:    "api_error",
			Message: message,
		}
	}
}

// decisiveLeg returns the failure that ended the chain: the last leg tried, which
// either aborted the call or exhausted the candidates.
func decisiveLeg(attempts []smolllm.Attempt) *smolllm.LegError {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].Failed() {
			return attempts[i].Err
		}
	}
	return nil
}

// isClientError reports whether the upstream blamed the request rather than
// itself. 429 is excluded: the chain advances past it, so it never aborts.
func isClientError(status int) bool {
	return status >= http.StatusBadRequest && status < http.StatusInternalServerError
}
