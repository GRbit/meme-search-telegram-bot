// Package configerr lets a module reject one field of its Config in a way a
// host can map back to its own setting names (env vars, YAML keys, flags).
//
// It is a separate leaf package so modules without Telegram code (memesearch)
// can use it without importing the bot library.
package configerr

import "errors"

// Field reports an invalid Config field. Name is the Go field name.
type Field struct {
	Name   string
	Reason string
}

func (e *Field) Error() string { return e.Name + ": " + e.Reason }

// Fields extracts every *Field from err, which may be a tree built with
// errors.Join and wrapped with %w; anything else is returned in rest.
func Fields(err error) (fields []*Field, rest []error) {
	if err == nil {
		return nil, nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range j.Unwrap() {
			f, r := Fields(e)
			fields, rest = append(fields, f...), append(rest, r...)
		}
		return fields, rest
	}
	if f, ok := err.(*Field); ok { //nolint:errorlint // wrappers are unwrapped explicitly below
		return []*Field{f}, nil
	}
	// Walk through single wrappers (fmt.Errorf with %w) so a join wrapped by a
	// constructor still yields every field, not only the first errors.As finds.
	if inner := errors.Unwrap(err); inner != nil {
		if fields, _ := Fields(inner); len(fields) > 0 {
			return Fields(inner)
		}
	}
	return nil, []error{err}
}
