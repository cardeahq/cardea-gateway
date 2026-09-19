package pipeline

import (
	"encoding/json"
	"net/http"
)

type denialEnvelope struct {
	Error struct {
		Type      string `json:"type"`
		Stage     string `json:"stage"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func writeDenial(w http.ResponseWriter, requestID string, d *Denial, unavailable bool) {
	env := denialEnvelope{}
	if unavailable {
		env.Error.Type = "agentkeeper_unavailable"
	} else {
		env.Error.Type = "agentkeeper_denied"
	}
	env.Error.Stage = d.Stage
	env.Error.Message = d.Message
	env.Error.RequestID = requestID

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Agentkeeper-Request-Id", requestID)
	w.WriteHeader(d.Status)
	_ = json.NewEncoder(w).Encode(env)
}
