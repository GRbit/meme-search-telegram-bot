package configerr

import (
	"errors"
	"fmt"
	"testing"
)

func TestFields(t *testing.T) {
	other := errors.New("other")
	err := errors.Join(
		&Field{Name: "A", Reason: "bad"},
		errors.Join(fmt.Errorf("wrapped: %w", &Field{Name: "B", Reason: "worse"}), other),
	)
	fields, rest := Fields(err)
	if len(fields) != 2 || fields[0].Name != "A" || fields[1].Name != "B" {
		t.Fatalf("fields = %+v", fields)
	}
	if len(rest) != 1 || !errors.Is(rest[0], other) {
		t.Fatalf("rest = %v", rest)
	}
	if f, r := Fields(nil); f != nil || r != nil {
		t.Fatal("nil error must yield nothing")
	}
}
