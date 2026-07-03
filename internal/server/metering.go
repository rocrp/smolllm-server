package server

import (
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/meter"
)

func (h *handlers) appendUsageHook(opts []smolllm.Option, alias string, stream bool) []smolllm.Option {
	path, err := h.cfg().UsagePath()
	if err != nil {
		h.logger.Warn("resolve usage path failed", "error", err)
		return opts
	}
	if path == "" {
		return opts
	}
	return append(opts, smolllm.WithHook(func(event smolllm.RequestEvent) {
		record := meter.Record{
			Timestamp:    event.Timestamp,
			Alias:        alias,
			Provider:     event.Provider,
			Model:        event.Model,
			ModelName:    event.ModelName,
			InputTokens:  event.InputTokens,
			OutputTokens: event.OutputTokens,
			Estimated:    event.Estimated,
			DurationMS:   event.Duration.Milliseconds(),
			TTFTMS:       event.TTFT.Milliseconds(),
			Stream:       stream,
			OK:           event.Error == nil,
		}
		if event.Timestamp.IsZero() {
			record.Timestamp = time.Now().UTC()
		}
		if event.Error != nil {
			record.Error = event.Error.Error()
		}
		if err := meter.Append(path, record); err != nil {
			h.logger.Warn("append usage record failed", "path", path, "error", err)
		}
	}))
}
