package config

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// maxDNSSuffixLength is the maximum length of the textual representation of a
// domain name (RFC 1035 section 2.3.4).
const maxDNSSuffixLength = 253

// ErrEmptySuffix is returned when the configured suffix has no usable label.
var ErrEmptySuffix = errors.New("must contain at least two labels, e.g. docker.local")

// NormalizeSuffix canonicalises a user supplied base domain so that every
// later comparison is a plain string equality.
//
// It lowercases the value, trims surrounding whitespace and dots, and rejects
// anything that is not a sequence of RFC 1123 labels. Normalising here means
// the rest of the program never has to think about case or trailing dots.
//
// The returned string is the reason on failure, so callers can wrap it in a
// FieldError. Use it as: suffix, err := NormalizeSuffix(raw).
func NormalizeSuffix(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.Trim(s, ".")

	if s == "" {
		return "", ErrEmptySuffix
	}
	if len(s) > maxDNSSuffixLength {
		return "", fmt.Errorf("must be at most %d characters", maxDNSSuffixLength)
	}

	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return "", ErrEmptySuffix
	}
	for _, label := range labels {
		if err := validateLabel(label); err != nil {
			return "", err
		}
	}
	return s, nil
}

// ErrInvalidLabel describes a malformed DNS label.
type ErrInvalidLabel struct {
	Label  string
	Reason string
}

func (e *ErrInvalidLabel) Error() string {
	return fmt.Sprintf("label %q %s", e.Label, e.Reason)
}

// validateLabel enforces RFC 1123 for a single label: 1-63 characters of
// [a-z0-9-], not starting or ending with a hyphen. The caller is expected to
// have lowercased the input already.
func validateLabel(label string) error {
	if label == "" {
		return &ErrInvalidLabel{"", "must not be empty"}
	}
	if len(label) > 63 {
		return &ErrInvalidLabel{label, "must be at most 63 characters"}
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return &ErrInvalidLabel{label, "must not start or end with a hyphen"}
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return &ErrInvalidLabel{label, "may only contain a-z, 0-9 and hyphens"}
		}
	}
	return nil
}

// normalizePath validates and cleans the hosts file path.
//
// It rejects relative paths on purpose: a relative path inside the container
// would silently manage the wrong file, and this agent has no business
// touching anything but an absolute, explicit location.
func normalizePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", errors.New("must not be empty")
	}
	if !strings.HasPrefix(p, "/") {
		return p, errors.New("must be an absolute path")
	}
	// path.Clean also collapses "..", which would let a stray value escape
	// the mounted directory.
	clean := path.Clean(p)
	if clean == "/" || clean == "/etc" {
		return p, fmt.Errorf("refusing to manage the whole directory %q", clean)
	}
	return clean, nil
}
