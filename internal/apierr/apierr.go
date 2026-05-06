package apierr

import (
	"encoding/json"
	"net/http"
)

// Body is the OpenAI-compatible error envelope: {"error": {"message", "type", "code"}}.
type Body struct {
	Error Detail `json:"error"`
}

type Detail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// Write serializes the error envelope at the given status code.
func Write(w http.ResponseWriter, status int, code, kind, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Body{Error: Detail{
		Message: message,
		Type:    kind,
		Code:    code,
	}})
}

// WriteParam is like Write but includes a `param` hint identifying the offending field.
func WriteParam(w http.ResponseWriter, status int, code, kind, message, param string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Body{Error: Detail{
		Message: message,
		Type:    kind,
		Code:    code,
		Param:   param,
	}})
}
