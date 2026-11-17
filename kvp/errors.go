// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"errors"
	"fmt"
)

var (
	ErrEmptyKey          = errors.New("KVP key must not be empty")
	ErrKeyContainsNull   = errors.New("KVP key must not contain null bytes")
	ErrValueContainsNull = errors.New("KVP value must not contain null bytes")
	ErrKeyTooLarge       = errors.New("KVP key exceeds maximum size")
	ErrValueTooLarge     = errors.New("KVP value exceeds maximum size")
	ErrTooManyKeys       = errors.New("KVP unique key count exceeds maximum")
	ErrInvalidUTF8       = errors.New("KVP text must be valid UTF-8")
	ErrInvalidPool       = errors.New("invalid KVP pool")
	ErrInvalidMode       = errors.New("invalid KVP size policy")
	ErrInvalidFraming    = errors.New("invalid KVP record framing")
	ErrUnsupportedOS     = errors.New("KVP file locking requires Linux")
	ErrEmptyField        = errors.New("diagnostic field must not be empty")
	ErrFieldDelimiter    = errors.New("diagnostic field must not contain '|'")
	ErrFieldTooLong      = errors.New("diagnostic field exceeds maximum size")
	ErrInvalidUUID       = errors.New("diagnostic identity must be a UUID")
	ErrTooManyChunks     = errors.New("diagnostic chunk count exceeds maximum")
	ErrPayloadNotUTF8    = errors.New("unencoded diagnostic payload must be UTF-8")
	ErrEncoding          = errors.New("unsupported diagnostic encoding")
	ErrInvalidDuration   = errors.New("invalid diagnostic duration")
)

// ValidationError identifies an invalid field or a byte/count limit.
// Use errors.Is with Err to classify it.
type ValidationError struct {
	Err    error
	Field  string
	Max    int
	Actual int
}

func (e *ValidationError) Error() string {
	if e.Max != 0 || e.Actual != 0 {
		return fmt.Sprintf("%s: field=%s actual=%d max=%d", e.Err, e.Field, e.Actual, e.Max)
	}
	return fmt.Sprintf("%s: field=%s", e.Err, e.Field)
}

func (e *ValidationError) Unwrap() error { return e.Err }

// DecodeError classifies a recognized record that could not be decoded.
// Such errors are attached to raw entries; they do not discard pool records.
type DecodeError string

const (
	UnsupportedVersion DecodeError = "unsupported_version"
	IncompleteGroup    DecodeError = "incomplete_group"
	DuplicateChunk     DecodeError = "duplicate_chunk"
	Undecodable        DecodeError = "undecodable"
	Malformed          DecodeError = "malformed"
)

func (e DecodeError) Error() string {
	switch e {
	case UnsupportedVersion:
		return "unsupported diagnostic schema version"
	case IncompleteGroup:
		return "diagnostic chunks are not contiguous from index 0"
	case DuplicateChunk:
		return "duplicate diagnostic chunk index"
	case Undecodable:
		return "diagnostic payload could not be decoded"
	case Malformed:
		return "malformed diagnostic or provisioning report"
	default:
		return fmt.Sprintf("unknown diagnostic decoding error %q", string(e))
	}
}
