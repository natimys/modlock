package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"modlock/internal/failure"
)

type fragmented struct{ io.Reader }

func (f fragmented) Read(b []byte) (int, error) {
	if len(b) > 2 {
		b = b[:2]
	}
	return f.Reader.Read(b)
}

func events(t *testing.T, output []byte) []Event {
	t.Helper()
	var result []Event
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte{'\n'}) {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("non-JSON stdout: %q: %v", line, err)
		}
		if event.Protocol != Protocol {
			t.Fatalf("missing protocol: %q", line)
		}
		result = append(result, event)
	}
	return result
}

func TestFragmentedRequestProgressAndStructuredError(t *testing.T) {
	var output bytes.Buffer
	input := fragmented{strings.NewReader("{\"type\":\"request\",\"id\":\"a\",\"operation\":\"check\"}\n")}
	err := Serve(context.Background(), input, &output, func(_ context.Context, request Request, progress func(string)) (any, error) {
		if request.Operation != "check" {
			t.Errorf("wrong operation: %q", request.Operation)
		}
		progress("Получение\nсборки")
		return nil, failure.Wrap(failure.Network, errors.New("offline"))
	})
	if err != nil {
		t.Fatal(err)
	}
	got := events(t, output.Bytes())
	if len(got) != 2 || got[0].Type != "progress" || got[0].Message != "Получение\nсборки" || got[1].Type != "result" || got[1].Error.Code != failure.Network {
		t.Fatalf("events: %#v", got)
	}
}

func TestCancellationWaitsForRestoration(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	var output bytes.Buffer
	started, restored := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), reader, &output, func(ctx context.Context, _ Request, _ func(string)) (any, error) {
			close(started)
			<-ctx.Done()
			// Model a handler that must restore its transaction before returning.
			close(restored)
			return nil, ctx.Err()
		})
	}()
	if _, err := fmt.Fprintln(writer, `{"type":"request","id":"a","operation":"apply"}`); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := fmt.Fprintln(writer, `{"type":"cancel","id":"a"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	select {
	case <-restored:
	default:
		t.Fatal("process ended before restoration")
	}
	got := events(t, output.Bytes())
	if len(got) != 1 || got[0].Error == nil || got[0].Error.Code != failure.Cancelled {
		t.Fatalf("events: %#v", got)
	}
}

func TestProtocolCompatibilityAndCapabilities(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		var output bytes.Buffer
		err := Run([]string{"--protocol", version, "--root", t.TempDir()}, strings.NewReader(`{"type":"request","id":"a","operation":"capabilities"}`), &output)
		if err != nil {
			t.Fatal(err)
		}
		got := events(t, output.Bytes())
		if len(got) != 1 || got[0].Type != "result" {
			t.Fatalf("events: %#v", got)
		}
		if version == "1" && (got[0].Error != nil || got[0].Result == nil) {
			t.Fatalf("capabilities: %#v", got)
		}
		if version == "2" && (got[0].Error == nil || got[0].Error.Code != failure.UnsupportedProtocol) {
			t.Fatalf("compatibility: %#v", got)
		}
	}
}

func TestMalformedAndOversizedMessages(t *testing.T) {
	for _, input := range []string{"{broken}\n", `{"type":"request","id":"a","operation":"check","unknown":true}`, strings.Repeat("x", MaxMessageBytes+1)} {
		var output bytes.Buffer
		called := false
		err := Serve(context.Background(), strings.NewReader(input), &output, func(context.Context, Request, func(string)) (any, error) { called = true; return nil, nil })
		if err != nil {
			t.Fatal(err)
		}
		got := events(t, output.Bytes())
		if called || len(got) != 1 || got[0].Error == nil || got[0].Error.Code != failure.InvalidRequest {
			t.Fatalf("events: %#v, called=%v", got, called)
		}
	}
}
