package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/capability"
	"github.com/fcordero/llm-api-gateway/internal/metrics"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/translate"
)

// maxUpstreamErrorMessage bounds provider text echoed to clients. Upstream
// bodies can contain stack traces, request excerpts, or credential fragments;
// keep enough to debug, never enough to leak a payload.
const maxUpstreamErrorMessage = 4 << 10

// errorBody is the wire shape of an OpenAI error. provider.Error has no param
// field and belongs to another package's ownership, so proxy serializes this
// local struct instead of widening the shared type.
type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param,omitempty"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// writeError writes an OpenAI-compatible error response.
func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeErrorWithDetails(w, status, errType, errType, message, "")
}

// writeErrorWithDetails writes an OpenAI-compatible error with an explicit
// code and parameter, used for validation and translation failures.
func writeErrorWithDetails(w http.ResponseWriter, status int, errType, code, message, param string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{
		Message: message,
		Type:    errType,
		Code:    code,
		Param:   param,
	}})
}

// writeDispatchError maps a dispatch/stream error to the closest honest HTTP
// status. Client-visible failures keep their meaning (bad request stays a 400,
// rate limit stays a 429) instead of collapsing into a 502 that callers cannot
// act on.
func writeDispatchError(w http.ResponseWriter, err error) {
	var ce *capability.MismatchError
	if errors.As(err, &ce) {
		for _, missing := range ce.Missing {
			metrics.CapabilityMismatches.WithLabelValues(ce.Provider, missing, "request_rejected").Inc()
		}
	}
	status, errType, code, message, param := mapDispatchError(err)
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		setRetryAfter(w, pe)
	}
	writeErrorWithDetails(w, status, errType, code, message, param)
}

func mapDispatchError(err error) (status int, errType, code, message, param string) {
	if errors.Is(err, provider.ErrNoProvider) {
		return http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error(), ""
	}
	var ce *capability.MismatchError
	if errors.As(err, &ce) {
		return http.StatusBadRequest, "invalid_request_error", "model_capability_mismatch", sanitizeUpstreamMessage(ce.Error()), ce.Param()
	}
	if errors.Is(err, context.Canceled) {
		return 499, "provider_error", "client_closed_request", "client closed the request", ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "provider_error", "request_timeout", "request timed out", ""
	}

	var te *translate.Error
	if errors.As(err, &te) {
		code := te.Code
		if code == "" {
			code = "invalid_request"
		}
		return http.StatusBadRequest, "invalid_request_error", code, sanitizeUpstreamMessage(te.Message), te.Parameter
	}

	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		status, code = providerErrorStatus(pe)
		errType = "provider_error"
		switch code {
		case "invalid_request_error", "model_not_found", "request_too_large":
			errType = "invalid_request_error"
		case "rate_limit_exceeded":
			errType = "rate_limit_error"
		}
		message = providerErrorMessageForClient(pe)
		return status, errType, code, message, ""
	}

	return http.StatusBadGateway, "provider_error", "provider_error", sanitizeUpstreamMessage(err.Error()), ""
}

// providerErrorStatus maps an upstream status onto the gateway contract.
// 4xx caused by the caller is preserved; upstream auth failures are a gateway
// problem (the caller's key is fine) so they become 502 without echoing the
// provider body; throttling and payload-too-large keep their own status.
func providerErrorStatus(pe *provider.ProviderError) (int, string) {
	switch pe.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return http.StatusBadRequest, "invalid_request_error"
	case http.StatusNotFound:
		return http.StatusNotFound, "model_not_found"
	case http.StatusRequestEntityTooLarge:
		return http.StatusRequestEntityTooLarge, "request_too_large"
	case http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "rate_limit_exceeded"
	case http.StatusUnauthorized, http.StatusForbidden:
		return http.StatusBadGateway, "provider_auth_error"
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return http.StatusServiceUnavailable, "provider_unavailable"
	default:
		if pe.StatusCode >= 500 {
			return http.StatusBadGateway, "provider_error"
		}
		if pe.StatusCode == 0 {
			return http.StatusBadGateway, "provider_error"
		}
		return http.StatusBadGateway, "provider_error"
	}
}

func providerErrorMessageForClient(pe *provider.ProviderError) string {
	switch pe.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		// Never echo upstream auth bodies: they can quote the key or an
		// internal account identifier.
		return "provider rejected the gateway credentials"
	}
	return sanitizeUpstreamMessage(pe.Message)
}

// sanitizeUpstreamMessage keeps a provider message diagnostic but bounded and
// single-line, so it cannot be used to smuggle control characters or a large
// upstream payload through the gateway's error envelope.
func sanitizeUpstreamMessage(message string) string {
	message = strings.ReplaceAll(message, "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > maxUpstreamErrorMessage {
		message = message[:maxUpstreamErrorMessage] + "..."
	}
	return message
}

// setRetryAfter mirrors a provider Retry-After hint. Seconds are rounded up and
// floored at one so a sub-second hint does not become "0", which clients treat
// as "retry immediately".
func setRetryAfter(w http.ResponseWriter, pe *provider.ProviderError) {
	if pe == nil {
		return
	}
	if pe.StatusCode != http.StatusTooManyRequests && pe.RetryAfter <= 0 {
		return
	}
	seconds := int64((pe.RetryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

// propagateRetryAfter writes Retry-After if err carries a ProviderError hint.
func propagateRetryAfter(w http.ResponseWriter, err error) {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		setRetryAfter(w, pe)
	}
}
