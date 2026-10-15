package filtering

// FilterError classifies a Redact failure; it never carries request
// content, matching policy.EvalError.
type FilterError struct {
	// Kind is one of "input", "internal".
	Kind string
}

func (e *FilterError) Error() string {
	return "filtering failed: " + e.Kind
}
