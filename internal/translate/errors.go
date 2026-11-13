package translate

import "fmt"

// Error marks a request that cannot be represented faithfully in a provider's
// native format. Callers should surface it as a client (4xx) error instead of
// sending a silently degraded upstream request.
type Error struct {
	Parameter string
	Code      string
	Message   string
}

func (e *Error) Error() string {
	if e.Parameter != "" {
		return fmt.Sprintf("%s: %s", e.Parameter, e.Message)
	}
	return e.Message
}

func invalidRequest(format string, args ...any) error {
	return &Error{Code: "invalid_request", Message: fmt.Sprintf(format, args...)}
}

func unsupportedParameter(parameter, detail string) error {
	return &Error{Parameter: parameter, Code: "unsupported_parameter", Message: detail}
}
