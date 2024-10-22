package utils

import (
	"encoding/json"
	"net/http"
)
type Response struct {
    Data    interface{} `json:"data,omitempty"`
    Error   string      `json:"error,omitempty"`
    Success bool        `json:"success"`
}

func WriteJSONResponse(w http.ResponseWriter, status int, response *Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}
