package policy

// EvalError never carries OPA's message: only a kind, so nothing derived
// from a request can reach a log or a record.
type EvalError struct {
	// Kind is one of "timeout", "overloaded", "internal",
	// "malformed_decision", "input".
	Kind string
}

func (e *EvalError) Error() string {
	return "policy evaluation failed: " + e.Kind
}
