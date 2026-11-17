// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"encoding/base64"
	"encoding/json"
	"unicode/utf8"
)

const (
	DiagnosticVersionID = "DIAG"
	MaxChunkBytes       = 1022
	MaxChunks           = 1023
)

type Kind string

const (
	Start  Kind = "start"
	Finish Kind = "finish"
	Event  Kind = "event"
)

type Encoding string

const (
	Plain      Encoding = "none"
	ZlibBase64 Encoding = "zlib+b64"
	GzipBase64 Encoding = "gz+b64"
)

func (e Encoding) token() string {
	if e == "" {
		return string(Plain)
	}
	return string(e)
}

type Outcome string

const (
	OK    Outcome = "ok"
	Error Outcome = "error"
)

// Payload retains the distinction between text and bytes. Compressed payloads
// always decode to bytes, even when their content happens to be valid text.
// The zero value is empty text.
type Payload struct {
	data   []byte
	binary bool
}

func TextPayload(text string) Payload { return Payload{data: []byte(text)} }
func BytesPayload(data []byte) Payload {
	return Payload{data: append([]byte(nil), data...), binary: true}
}
func (p Payload) Text() (string, bool) { return string(p.data), !p.binary }
func (p Payload) Bytes() []byte        { return append([]byte(nil), p.data...) }
func (p Payload) IsBinary() bool       { return p.binary }

func (p Payload) MarshalJSON() ([]byte, error) {
	if !p.binary {
		if !utf8.Valid(p.data) {
			return nil, ErrPayloadNotUTF8
		}
		return json.Marshal(string(p.data))
	}
	return json.Marshal(struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Data     string `json:"data"`
	}{"bytes", "base64", base64.StdEncoding.EncodeToString(p.data)})
}

type DiagnosticKey struct {
	Agent     string
	VMID      *string
	Name      string
	EventID   string
	Timestamp Timestamp
	Encoding  Encoding
}

// TimestampString returns UTC with automatic 0/3/6/9 fractional digits.
func (k DiagnosticKey) TimestampString() string {
	return k.Timestamp.String()
}

// Diagnostic represents a start, finish, or standalone event. Starts have no
// result or duration; finishes require both. Events permit either independently.
type Diagnostic struct {
	Kind Kind
	DiagnosticKey
	Payload  Payload
	Result   *Outcome
	Duration *Duration
}

func (d Diagnostic) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Kind      Kind      `json:"kind"`
		Agent     string    `json:"agent"`
		VMID      *string   `json:"vm_id"`
		Name      string    `json:"name"`
		EventID   string    `json:"event_id"`
		Timestamp string    `json:"timestamp"`
		Encoding  string    `json:"encoding"`
		Payload   Payload   `json:"payload"`
		Result    *Outcome  `json:"result,omitempty"`
		Duration  *Duration `json:"duration,omitempty"`
	}{d.Kind, d.Agent, d.VMID, d.Name, d.EventID, d.TimestampString(),
		d.Encoding.token(), d.Payload, d.Result, d.Duration})
}

type RawKeyValue struct {
	Key   string      `json:"key"`
	Value string      `json:"value"`
	Error DecodeError `json:"error,omitempty"`
}

// Entry contains exactly one decoded diagnostic, report, or preserved raw
// record. MarshalJSON matches the upstream tagged JSON representation.
type Entry struct {
	Diagnostic *Diagnostic
	Report     *ProvisioningReport
	Raw        *RawKeyValue
}

func (e Entry) MarshalJSON() ([]byte, error) {
	count := 0
	var tag string
	var value interface{}
	if e.Diagnostic != nil {
		count++
		tag, value = "diagnostic", e.Diagnostic
	}
	if e.Report != nil {
		count++
		tag, value = ProvisioningReportKey, e.Report
	}
	if e.Raw != nil {
		count++
		tag, value = "raw", e.Raw
	}
	if count != 1 {
		return nil, Malformed
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	// All three variants encode nonempty objects, and these tags are constants.
	return append([]byte(`{"type":"`+tag+`",`), data[1:]...), nil
}
