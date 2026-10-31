package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/rocry/smolllm-server/internal/apierr"
	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/llm"
)

const (
	// maxImageRequestBytes caps the inbound body: an image prompt is text.
	maxImageRequestBytes = 1 << 20
	// maxImageReplyBytes is smolayer's own bound on an image reply. A larger one
	// is refused, never relayed truncated.
	maxImageReplyBytes = 64 << 20
)

// images forwards one image request to the one smolayer leg its alias names.
// Only `model` is rewritten; smolayer owns validation of every other field. The
// call is never retried and never falls back: a failed generation has already
// spent quota, and the client sees smolayer's own status and body.
func (h *handlers) images(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImageRequestBytes))
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			apierr.Write(w, http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error",
				fmt.Sprintf("request body exceeds %d bytes", maxImageRequestBytes))
			return
		}
		badRequest(w, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		badRequest(w, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	var alias string
	if raw, ok := fields["model"]; ok {
		if err := json.Unmarshal(raw, &alias); err != nil {
			badRequest(w, "model must be a string")
			return
		}
	}
	if strings.TrimSpace(alias) == "" {
		badRequest(w, "model is required")
		return
	}
	spec, ok := h.cfg().ImageModel(alias)
	if !ok {
		badRequest(w, fmt.Sprintf("%q is not an image alias; /v1/images/generations takes only the names under image_aliases", alias))
		return
	}

	note := noteFor(r)
	note.alias, note.model = alias, spec

	baseURL, apiKey := os.Getenv(envSmolayerBaseURL), os.Getenv(envSmolayerAPIKey)
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(apiKey) == "" {
		apierr.Write(w, http.StatusInternalServerError, "internal_error", "server_error",
			fmt.Sprintf("%s and %s must be set to serve images", envSmolayerBaseURL, envSmolayerAPIKey))
		return
	}
	fields["model"], _ = json.Marshal(strings.TrimPrefix(spec, config.ImageProvider+"/"))
	forward, err := json.Marshal(fields)
	if err != nil {
		badRequest(w, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg().Server.ImageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, imagesEndpoint(baseURL), bytes.NewReader(forward))
	if err != nil {
		apierr.Write(w, http.StatusInternalServerError, "internal_error", "server_error", err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	req.Header.Set("Content-Type", "application/json")
	if id := r.Header.Get("X-Request-ID"); id != "" {
		req.Header.Set("X-Request-ID", id)
	}

	reply, status, contentType, err := roundTripBounded(req)
	if err != nil {
		h.writeImageTransportFailure(w, r, err)
		return
	}
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	if _, err := w.Write(reply); err != nil {
		h.logger.Warn("write images response failed", "error", err)
	}
}

var errImageReplyTooLarge = fmt.Errorf("smolayer reply exceeds %d MiB", maxImageReplyBytes>>20)

func roundTripBounded(req *http.Request) (reply []byte, status int, contentType string, err error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	reply, err = io.ReadAll(io.LimitReader(resp.Body, maxImageReplyBytes+1))
	if err != nil {
		return nil, 0, "", err
	}
	if len(reply) > maxImageReplyBytes {
		return nil, 0, "", errImageReplyTooLarge
	}
	return reply, resp.StatusCode, resp.Header.Get("Content-Type"), nil
}

// writeImageTransportFailure reports a call that produced no reply from smolayer
// at all; a reply of any status, errors included, is relayed instead.
func (h *handlers) writeImageTransportFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case r.Context().Err() != nil:
		noteFor(r).status = llm.StatusClientClosedRequest
		w.WriteHeader(llm.StatusClientClosedRequest)
	case errors.Is(err, context.DeadlineExceeded):
		apierr.Write(w, http.StatusGatewayTimeout, "image_timeout", "api_error",
			fmt.Sprintf("image request exceeded server.image_timeout (%s)", h.cfg().Server.ImageTimeout))
	default:
		apierr.Write(w, http.StatusBadGateway, "upstream_error", "api_error", err.Error())
	}
}

const (
	envSmolayerBaseURL = "SMOLAYER_BASE_URL"
	envSmolayerAPIKey  = "SMOLAYER_API_KEY"
)

var versionSuffix = regexp.MustCompile(`/v\d+$`)

// imagesEndpoint builds smolayer's URL the way smolllm-go builds its chat one:
// a base ending in a version segment gets the path appended, any other base
// gets /v1 first.
func imagesEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if versionSuffix.MatchString(base) {
		return base + "/images/generations"
	}
	return base + "/v1/images/generations"
}
