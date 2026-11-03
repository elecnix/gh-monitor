package backend_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/elecnix/gh-monitor/backend"
)

// TestRegisterCapabilityRejectsWrongType is the guard on the untyped
// registration entry point. RegisterCapability returns an error and its
// contract says which interface an implementation must satisfy, so a value of
// the wrong Go type has to be reported as an error. The two capabilities that
// assert their implementation type — source and reader — used to do it with
// a comma-less type assertion, so a wrong-typed value panicked instead of
// returning.
//
// A panic here is not a crash the caller can handle: the remote client, the
// daemon and the CLI all register backends at startup, and a panic there
// takes the whole process down rather than reporting a bad backend.
func TestRegisterCapabilityRejectsWrongType(t *testing.T) {
	cases := []struct {
		capability backend.Capability
		impl       any
	}{
		{backend.CapSource, "not a Source"},
		{backend.CapSource, 42},
		{backend.CapReader, "not a Reader"},
		{backend.CapReader, 42},
	}
	for _, tc := range cases {
		t.Run(string(tc.capability), func(t *testing.T) {
			r := backend.NewRegistry()
			err := r.RegisterCapability(tc.capability, "relay", nil, tc.impl)
			if err == nil {
				t.Fatalf("RegisterCapability(%s, %T) = nil error, want an error", tc.capability, tc.impl)
			}
			if !errors.Is(err, backend.ErrNoBackend) {
				t.Errorf("RegisterCapability(%s, %T) error = %v, want one wrapping ErrNoBackend",
					tc.capability, tc.impl, err)
			}
			if !strings.Contains(err.Error(), "wrong type") {
				t.Errorf("RegisterCapability(%s, %T) error = %v, want it to say the type is wrong",
					tc.capability, tc.impl, err)
			}
		})
	}
}

// TestRegisterCapabilityRejectsNilType covers the nil case separately, because
// a nil implementation is reported differently from a wrong-typed one: a
// caller that passes nil has registered nothing at all, not the wrong thing.
func TestRegisterCapabilityRejectsNilType(t *testing.T) {
	r := backend.NewRegistry()
	err := r.RegisterCapability(backend.CapSource, "relay", nil, nil)
	if err == nil {
		t.Fatal("RegisterCapability(source, nil) = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("RegisterCapability(source, nil) error = %v, want it to say the implementation is nil", err)
	}
}

// TestRegisterCapabilityAcceptsCorrectType is the other half: the guard must
// not reject a value that does satisfy the interface, or the fix would trade
// a panic for a false error.
func TestRegisterCapabilityAcceptsCorrectType(t *testing.T) {
	r := backend.NewRegistry()
	if err := r.RegisterCapability(backend.CapSource, "relay", nil, backend.SourceFunc(
		func(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
			ch := make(chan backend.Update)
			close(ch)
			return ch, nil
		})); err != nil {
		t.Fatalf("RegisterCapability(source, a valid Source) = %v, want nil", err)
	}
	if err := r.RegisterCapability(backend.CapReader, "relay", nil, backend.ReaderFunc(
		func(ctx context.Context, t backend.Target) (backend.Status, error) {
			return nil, nil
		})); err != nil {
		t.Fatalf("RegisterCapability(reader, a valid Reader) = %v, want nil", err)
	}
}
