package region

import (
	"errors"
	"fmt"
	"net/http"
)

// Errors a control plane reports to an edge. They cross the wire as an error
// code (see the table below), and Client turns the code back into the same
// sentinel, so errors.Is works on both sides of the connection.
var (
	ErrUnauthorized          = errors.New("unauthorized")
	ErrNotFound              = errors.New("not found")
	ErrRegionMismatch        = errors.New("request names a region the caller is not authenticated for")
	ErrLeaseInsufficient     = errors.New("regional lease balance is insufficient")
	ErrOrganizationForbidden = errors.New("regional organization access is forbidden")
	ErrOrganizationLimit     = errors.New("regional organization limit exceeded")
	ErrMemberLimit           = errors.New("regional organization member limit exceeded")
	ErrApiKeyCostLimit       = errors.New("regional api key cost limit exceeded")
)

// Error codes of the control-plane API. Every non-2xx response carries one in
// {"error":{"code":"...","message":"..."}}.
const (
	CodeUnauthorized          = "unauthorized"
	CodeInvalidRequest        = "invalid_request"
	CodeNotFound              = "not_found"
	CodeRegionMismatch        = "region_mismatch"
	CodeInsufficientBalance   = "insufficient_balance"
	CodeOrganizationForbidden = "organization_forbidden"
	CodeOrganizationLimit     = "organization_limit_exceeded"
	CodeMemberLimit           = "member_limit_exceeded"
	CodeAPIKeyCostLimit       = "api_key_cost_limit_exceeded"
	CodeUnavailable           = "unavailable"
	CodeInternal              = "internal_error"
)

var errorCodes = []struct {
	err    error
	status int
	code   string
}{
	{ErrUnauthorized, http.StatusUnauthorized, CodeUnauthorized},
	{ErrNotFound, http.StatusNotFound, CodeNotFound},
	{ErrRegionMismatch, http.StatusForbidden, CodeRegionMismatch},
	{ErrLeaseInsufficient, http.StatusPaymentRequired, CodeInsufficientBalance},
	{ErrOrganizationForbidden, http.StatusForbidden, CodeOrganizationForbidden},
	{ErrOrganizationLimit, http.StatusPaymentRequired, CodeOrganizationLimit},
	{ErrMemberLimit, http.StatusPaymentRequired, CodeMemberLimit},
	{ErrApiKeyCostLimit, http.StatusPaymentRequired, CodeAPIKeyCostLimit},
}

// StatusAndCode maps an error to the HTTP status and code a control plane
// answers with. Errors outside the table are internal errors.
func StatusAndCode(err error) (int, string) {
	for _, e := range errorCodes {
		if errors.Is(err, e.err) {
			return e.status, e.code
		}
	}
	return http.StatusInternalServerError, CodeInternal
}

// ErrorResponse is the body of every non-2xx control-plane response.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error is a non-2xx control-plane response as the client sees it.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("region control plane returned %d %s: %s", e.Status, e.Code, e.Message)
}

// Is matches the sentinel that the error code stands for.
func (e *Error) Is(target error) bool {
	for _, c := range errorCodes {
		if c.code == e.Code {
			return c.err == target
		}
	}
	return false
}
