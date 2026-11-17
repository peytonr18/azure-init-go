// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type writerConfig struct {
	maxNameBytes       int
	timestampPrecision Precision
	durationPrecision  Precision
}

type WriterOption func(*writerConfig) error

func WithMaxNameBytes(max int) WriterOption {
	return func(config *writerConfig) error {
		if max < 0 {
			return fmt.Errorf("maximum name bytes must be nonnegative")
		}
		config.maxNameBytes = max
		return nil
	}
}

func WithTimestampPrecision(precision Precision) WriterOption {
	return func(config *writerConfig) error {
		if !validPrecision(precision) {
			return fmt.Errorf("invalid timestamp precision %d", precision)
		}
		config.timestampPrecision = precision
		return nil
	}
}

func WithDurationPrecision(precision Precision) WriterOption {
	return func(config *writerConfig) error {
		if !validPrecision(precision) {
			return fmt.Errorf("invalid duration precision %d", precision)
		}
		config.durationPrecision = precision
		return nil
	}
}

// DiagnosticWriter appends native diagnostics for one agent and VM.
// All chunks are validated before being appended under one exclusive lock.
// The writer never clears records or performs HTTP reporting.
type DiagnosticWriter struct {
	store  *Store
	agent  string
	vmID   string
	config writerConfig
	now    func() time.Time
	newID  func() (string, error)
}

func NewDiagnosticWriter(store *Store, agent, vmID string, options ...WriterOption) (*DiagnosticWriter, error) {
	if store == nil {
		return nil, fmt.Errorf("diagnostic writer requires a store")
	}
	if err := validateField("agent", agent, 32); err != nil {
		return nil, err
	}
	if err := validateUUIDField("vm_id", vmID); err != nil {
		return nil, err
	}
	config := writerConfig{maxNameBytes: 64, timestampPrecision: Milliseconds, durationPrecision: Microseconds}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("nil diagnostic writer option")
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	return &DiagnosticWriter{store: store, agent: agent, vmID: vmID, config: config, now: time.Now, newID: randomUUID}, nil
}

func (w *DiagnosticWriter) EmitStart(eventID, name string, payload Payload, encoding Encoding) error {
	return w.emit(Start, eventID, name, payload, encoding, nil, nil, w.now())
}

func (w *DiagnosticWriter) EmitFinish(eventID, name string, payload Payload, encoding Encoding, result Outcome, duration Duration) error {
	return w.emit(Finish, eventID, name, payload, encoding, &result, &duration, w.now())
}

func (w *DiagnosticWriter) EmitEvent(name string, payload Payload, encoding Encoding, result *Outcome, duration *Duration) error {
	timestamp := w.now()
	eventID, err := w.newID()
	if err != nil {
		return fmt.Errorf("generate diagnostic event UUID: %w", err)
	}
	return w.emit(Event, eventID, name, payload, encoding, result, duration, timestamp)
}

func (w *DiagnosticWriter) emit(kind Kind, eventID, name string, payload Payload, encoding Encoding, result *Outcome, duration *Duration, capturedAt time.Time) error {
	if err := validateField("name", name, w.config.maxNameBytes); err != nil {
		return err
	}
	if err := validateUUIDField("event_id", eventID); err != nil {
		return err
	}
	if result != nil && *result != OK && *result != Error {
		return Malformed
	}
	formattedDuration := ""
	if duration != nil {
		var err error
		formattedDuration, err = duration.Format(w.config.durationPrecision)
		if err != nil {
			return err
		}
	}
	timestamp := formatTimestamp(capturedAt, w.config.timestampPrecision, true)
	if err := validateField("timestamp", timestamp, 30); err != nil {
		return err
	}
	value, err := encodePayload(payload, encoding)
	if err != nil {
		return err
	}
	outcome := ""
	if result != nil {
		outcome = string(*result)
	}
	base := strings.Join([]string{DiagnosticVersionID, w.agent, w.vmID, string(kind), name, eventID,
		timestamp, encoding.token(), outcome, formattedDuration}, "|")
	capacity := len(value)/MaxChunkBytes + 1
	if capacity > MaxChunks {
		capacity = MaxChunks
	}
	records := make([]Record, 0, capacity)
	for {
		if len(records) == MaxChunks {
			return &ValidationError{Err: ErrTooManyChunks, Field: "chunks", Max: MaxChunks, Actual: len(records) + 1}
		}
		key := fmt.Sprintf("%s|%d", base, len(records))
		if len(key) > SafeMaxKeyBytes {
			return &ValidationError{Err: ErrKeyTooLarge, Field: "key", Max: SafeMaxKeyBytes, Actual: len(key)}
		}
		end := len(value)
		if end > MaxChunkBytes {
			end = MaxChunkBytes
			for !utf8.RuneStart(value[end]) {
				end--
			}
		}
		records = append(records, Record{key, value[:end]})
		value = value[end:]
		if value == "" {
			return w.store.AppendMultiple(records)
		}
	}
}

func validateField(field, value string, max int) error {
	var err error
	switch {
	case value == "":
		err = ErrEmptyField
	case strings.ContainsRune(value, '|'):
		err = ErrFieldDelimiter
	case strings.ContainsRune(value, 0):
		return ErrKeyContainsNull
	case !utf8.ValidString(value):
		return ErrInvalidUTF8
	case len(value) > max:
		return &ValidationError{Err: ErrFieldTooLong, Field: field, Max: max, Actual: len(value)}
	default:
		return nil
	}
	return &ValidationError{Err: err, Field: field}
}

func validateUUIDField(field, value string) error {
	if err := validateField(field, value, 36); err != nil {
		return err
	}
	if _, err := parseUUID(value); err != nil {
		return &ValidationError{Err: ErrInvalidUUID, Field: field}
	}
	return nil
}
