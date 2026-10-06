package trace

import "errors"

// MultiSink writes each record to every one of its sinks.
type MultiSink struct{ sinks []CloseableSink }

func NewMultiSink(sinks ...CloseableSink) *MultiSink {
	return &MultiSink{sinks: sinks}
}

// Write offers the record to every sink, in order, and joins their errors.
func (m *MultiSink) Write(r Record) error {
	var errs []error
	for _, s := range m.sinks {
		if err := s.Write(r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close closes every sink and joins their errors.
func (m *MultiSink) Close() error {
	var errs []error
	for _, s := range m.sinks {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
