// Package bridge implements protocol-1 NDJSON process communication.
package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"modlock/internal/failure"
)

const Protocol = 1
const MaxMessageBytes = 1024 * 1024

type Request struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Operation string          `json:"operation,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Event struct {
	Protocol int    `json:"protocol"`
	Type     string `json:"type"`
	ID       string `json:"id"`
	Message  string `json:"message,omitempty"`
	Result   any    `json:"result,omitempty"`
	Error    *Error `json:"error,omitempty"`
}

type Handler func(context.Context, Request, func(string)) (any, error)

func decode(line []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("one JSON value is required per line")
	}
	return nil
}

func terminal(id string, result any, err error) Event {
	event := Event{Protocol: Protocol, Type: "result", ID: id, Result: result}
	if err != nil {
		event.Result = nil
		event.Error = &Error{Code: failure.Code(err), Message: err.Error()}
	}
	return event
}

// Serve processes one request plus optional cancellation messages. EOF after
// the request is allowed. It waits for the handler to finish, including rollback.
// A closable input is owned by Serve and closed on return to stop its reader.
func Serve(ctx context.Context, input io.Reader, output io.Writer, handle Handler) error {
	if closer, ok := input.(io.Closer); ok {
		defer closer.Close()
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), MaxMessageBytes)
	encoder := json.NewEncoder(output)
	if !scanner.Scan() {
		err := failure.Wrap(failure.InvalidRequest, fmt.Errorf("request is missing or exceeds the message limit"))
		if scanner.Err() != nil {
			err = failure.Wrap(failure.InvalidRequest, scanner.Err())
		}
		return encoder.Encode(terminal("", nil, err))
	}
	var request Request
	if err := decode(scanner.Bytes(), &request); err != nil {
		return encoder.Encode(terminal("", nil, failure.Wrap(failure.InvalidRequest, err)))
	}
	if request.Type != "request" || request.ID == "" || request.Operation == "" {
		return encoder.Encode(terminal(request.ID, nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("request type, id, and operation are required"))))
	}
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	var writeErr error
	emit := func(event Event) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if writeErr == nil {
			writeErr = encoder.Encode(event)
			if writeErr != nil {
				cancel()
			}
		}
	}
	progress := func(message string) {
		emit(Event{Protocol: Protocol, Type: "progress", ID: request.ID, Message: message})
	}
	type outcome struct {
		result any
		err    error
	}
	completed := make(chan outcome, 1)
	go func() { result, err := handle(opCtx, request, progress); completed <- outcome{result, err} }()
	type control struct {
		request Request
		err     error
	}
	controls := make(chan control, 1)
	stopReader := make(chan struct{})
	defer close(stopReader)
	go func() {
		defer close(controls)
		for scanner.Scan() {
			var message Request
			err := decode(scanner.Bytes(), &message)
			select {
			case controls <- control{message, err}:
			case <-stopReader:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case controls <- control{err: err}:
			case <-stopReader:
			}
		}
	}()
	var controlErr error
	for {
		select {
		case message, ok := <-controls:
			if !ok {
				controls = nil
				continue
			}
			if message.err != nil || message.request.Type != "cancel" || message.request.ID != request.ID || message.request.Operation != "" || len(message.request.Params) != 0 {
				controlErr = failure.Wrap(failure.InvalidRequest, fmt.Errorf("only cancellation for the active request is allowed"))
				cancel()
			} else {
				cancel()
			}
		case done := <-completed:
			if controlErr != nil {
				done.err = controlErr
			}
			emit(terminal(request.ID, done.result, done.err))
			writeMu.Lock()
			err := writeErr
			writeMu.Unlock()
			return err
		}
	}
}
