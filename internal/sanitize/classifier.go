package sanitize

// Span describes a sensitive substring detected within a text.
type Span struct {
	Start int     // byte offset of the first character (UTF-8)
	End   int     // byte offset one past the last character
	Label string  // e.g. "PER", "ORG", "MONEY", "CREDENTIAL", "CONFIDENTIAL"
	Score float32 // confidence in [0,1]; 1.0 for rule-based detectors
}

// Classifier detects sensitive spans in a text string.
// Implementations must be safe for concurrent use.
type Classifier interface {
	Classify(text string) ([]Span, error)
}
