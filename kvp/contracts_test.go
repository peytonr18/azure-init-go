// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestErrorContracts(t *testing.T) {
	for code, want := range map[DecodeError]string{
		UnsupportedVersion: "unsupported diagnostic schema version",
		IncompleteGroup:    "diagnostic chunks are not contiguous from index 0",
		DuplicateChunk:     "duplicate diagnostic chunk index",
		Undecodable:        "diagnostic payload could not be decoded",
		Malformed:          "malformed diagnostic or provisioning report",
		"future":           `unknown diagnostic decoding error "future"`,
	} {
		if code.Error() != want {
			t.Fatal(code.Error(), want)
		}
	}
	for _, value := range []ValidationError{
		{Err: ErrEmptyField, Field: "agent"},
		{Err: ErrFieldTooLong, Field: "name", Max: 0, Actual: 1},
		{Err: ErrKeyTooLarge, Field: "key", Max: 254, Actual: 255},
	} {
		if !errors.Is(&value, value.Err) || !strings.Contains(value.Error(), "field="+value.Field) {
			t.Fatal(value.Error())
		}
		if value.Actual != 0 && !strings.Contains(value.Error(), "actual=") {
			t.Fatal(value.Error())
		}
	}
}

func TestJSONModelContracts(t *testing.T) {
	stamp, err := parseTimestamp("2026-06-17T01:02:03.123Z")
	requireNoError(t, err)
	duration := Duration{Seconds: 1, Nanoseconds: 500000000}
	result := OK
	diagnostic := Diagnostic{Kind: Event, DiagnosticKey: DiagnosticKey{
		Agent: "agent", Name: "event", EventID: "id", Timestamp: stamp,
	}, Payload: TextPayload("text"), Duration: &duration, Result: &result}
	report := NewSuccessReport("agent", "vm", PPSNone)
	for _, entry := range []Entry{
		{Diagnostic: &diagnostic}, {Report: &report}, {Raw: &RawKeyValue{Key: "key", Value: "value"}},
	} {
		data, err := json.Marshal(entry)
		requireNoError(t, err)
		var decoded map[string]interface{}
		requireNoError(t, json.Unmarshal(data, &decoded))
		if decoded["type"] == nil {
			t.Fatal(string(data))
		}
	}
	data, err := stamp.MarshalJSON()
	requireNoError(t, err)
	if string(data) != `"2026-06-17T01:02:03.123Z"` {
		t.Fatal(string(data))
	}
	if Encoding("").token() != "none" {
		t.Fatal("zero encoding must be plain")
	}
	for _, entry := range []Entry{{}, {Report: &report, Diagnostic: &diagnostic}, {Report: &report, Raw: &RawKeyValue{}}} {
		if _, err := entry.MarshalJSON(); !errors.Is(err, Malformed) {
			t.Fatal(err)
		}
	}
	bad := diagnostic
	bad.Payload = TextPayload(string([]byte{0xff}))
	if _, err := (Entry{Diagnostic: &bad}).MarshalJSON(); !errors.Is(err, ErrPayloadNotUTF8) {
		t.Fatal(err)
	}
	bad = diagnostic
	invalid := Duration{Nanoseconds: 1_000_000_000}
	bad.Duration = &invalid
	if _, err := json.Marshal(Entry{Diagnostic: &bad}); !errors.Is(err, ErrInvalidDuration) {
		t.Fatal(err)
	}
	binary := BytesPayload([]byte{1, 2, 3})
	copy := binary.Bytes()
	copy[0] = 9
	if binary.Bytes()[0] != 1 {
		t.Fatal("payload bytes escaped without a copy")
	}
}

type failingOutput struct{}

func (failingOutput) Write([]byte) (int, error) { return 0, errInjectedIO }

func TestEncodingOutputFailures(t *testing.T) {
	data := make([]byte, 128*1024)
	_, err := rand.New(rand.NewSource(42)).Read(data)
	requireNoError(t, err)
	for _, encoding := range []Encoding{Plain, ZlibBase64, GzipBase64} {
		payloads := []Payload{TextPayload("small")}
		if encoding != Plain {
			payloads = append(payloads, BytesPayload(data))
		}
		for _, payload := range payloads {
			if err := encodePayloadTo(failingOutput{}, payload, encoding); !errors.Is(err, errInjectedIO) {
				t.Fatal(encoding, err)
			}
		}
	}
	if _, err := decodePayload(string([]byte{0xff}), Plain); !errors.Is(err, Undecodable) {
		t.Fatal(err)
	}
	if _, err := decodePayload("AAAA", "unsupported"); !errors.Is(err, Undecodable) {
		t.Fatal(err)
	}
	if _, err := decompress(nil, "unsupported"); !errors.Is(err, Undecodable) {
		t.Fatal(err)
	}
}

func TestUUIDEntropyAndIdentityErrors(t *testing.T) {
	uuid, err := randomUUIDFrom(bytes.NewReader(bytes.Repeat([]byte{0x11}, 16)))
	requireNoError(t, err)
	if uuid != "11111111-1111-4111-9111-111111111111" {
		t.Fatal(uuid)
	}
	if _, err := randomUUIDFrom(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	for _, value := range []string{
		"00112233_4455-6677-8899-aabbccddeeff",
		"00112233445566778899aabbccddeefg",
	} {
		if _, err := parseUUID(value); !errors.Is(err, ErrInvalidUUID) {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "uuid")
	sys, dev := filepath.Join(dir, "sys"), filepath.Join(dir, "dev")
	if _, err := currentVMID(path, sys, dev); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	requireNoError(t, os.WriteFile(path, []byte{0xff}, 0600))
	if _, err := currentVMID(path, sys, dev); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatal(err)
	}
	requireNoError(t, os.WriteFile(path, []byte("invalid-uuid"), 0600))
	if _, err := currentVMID(path, sys, dev); !errors.Is(err, ErrInvalidUUID) {
		t.Fatal(err)
	}
	requireNoError(t, os.WriteFile(path, []byte(testVM), 0600))
	requireNoError(t, os.Symlink(sys, sys))
	if _, err := currentVMID(path, sys, dev); err == nil {
		t.Fatal("EFI lookup error was swallowed")
	}
	// The public lookup must either return an identity or surface a host-specific
	// error; this assertion does not depend on running inside a Hyper-V guest.
	if value, err := CurrentVMID(); err == nil && value == "" {
		t.Fatal("empty identity without an error")
	}
}

func TestReportInvalidGoValuesAndRequiredFields(t *testing.T) {
	base := NewSuccessReport("agent", "vm", PPSNone)
	for _, mutate := range []func(*ProvisioningReport){
		func(r *ProvisioningReport) { r.PPSType = "unsupported" },
		func(r *ProvisioningReport) { r.Result = "unsupported" },
		func(r *ProvisioningReport) { r.Result = ResultError; r.Reason = nil },
		func(r *ProvisioningReport) { r.Agent = string([]byte{0xff}) },
	} {
		report := base
		mutate(&report)
		if _, err := report.Encode(); err == nil {
			t.Fatal("invalid Go report accepted")
		}
		if err := WriteReport(testStore(t, Safe), report); err == nil {
			t.Fatal("invalid report published")
		}
	}
	base.Timestamp = testTimestamp
	encoded, err := base.Encode()
	requireNoError(t, err)
	for _, field := range []string{"result", "agent", "vm_id", "timestamp", "pps_type"} {
		parts := strings.Split(encoded, "|")
		kept := make([]string, 0)
		for _, part := range parts {
			if !strings.HasPrefix(part, field+"=") {
				kept = append(kept, part)
			}
		}
		if _, err := ParseProvisioningReport(strings.Join(kept, "|")); !errors.Is(err, Malformed) {
			t.Fatal(field, err)
		}
	}
	if _, err := ParseProvisioningReport(string([]byte{0xff})); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
}

func TestNativeMetadataValidation(t *testing.T) {
	base := strings.Split(nativeKey("event", "none", "", "", "0"), "|")
	for _, field := range []int{1, 2, 4, 5, 6, 7} {
		fields := append([]string(nil), base...)
		fields[field] = ""
		entry := DecodeEntries([]Record{{strings.Join(fields, "|"), "value"}})[0]
		if entry.Raw == nil || entry.Raw.Error != Malformed {
			t.Fatal(field, entry)
		}
	}
	for _, key := range []string{"DIAG", "DIAG|bad|0", strings.Replace(strings.Join(base, "|"), testVM, "invalid", 1)} {
		entry := DecodeEntries([]Record{{key, "value"}})[0]
		if entry.Raw == nil || entry.Raw.Error != Malformed {
			t.Fatal(entry)
		}
	}
	report := NewSuccessReport("agent", "vm", PPSNone)
	report.Timestamp = testTimestamp
	encoded, err := report.Encode()
	requireNoError(t, err)
	entries := DecodeEntries([]Record{{ProvisioningReportKey, encoded}, {ProvisioningReportKey, "invalid"}})
	if entries[0].Report == nil || entries[1].Raw == nil || entries[1].Raw.Error != Malformed {
		t.Fatal(entries)
	}
	writer, store := testWriter(t)
	invalidResult := Outcome("warn")
	if err := writer.EmitEvent("op", TextPayload(""), Plain, &invalidResult, nil); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
	if _, err := NewDiagnosticWriter(nil, "agent", testVM); err == nil {
		t.Fatal("nil store accepted")
	}
	for _, value := range []string{"nul\x00name", string([]byte{0xff})} {
		if err := writer.EmitEvent(value, TextPayload(""), Plain, nil, nil); err == nil {
			t.Fatal("invalid field accepted")
		}
	}
	writer.now = func() time.Time { return time.Date(10000, 1, 1, 0, 0, 0, 1, time.UTC) }
	writer.config.timestampPrecision = Nanoseconds
	if err := writer.EmitStart(testEvent, "name", TextPayload(""), Plain); !errors.Is(err, ErrFieldTooLong) {
		t.Fatal(err)
	}
	recordsEqual(t, store, []Record{})
}

func TestJSONNumberAndEscapeContracts(t *testing.T) {
	for _, test := range []struct {
		input json.Number
		want  interface{}
	}{
		{"1", uint64(1)}, {"-1", int64(-1)}, {"-0", math.Copysign(0, -1)}, {"1.25", float64(1.25)},
		{"18446744073709551616", float64(18446744073709551616.0)},
	} {
		got, err := normalizeJSONNumbers(test.input)
		requireNoError(t, err)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatal(got, test.want)
		}
	}
	values := []interface{}{
		json.Number("1e9999"), json.Number("NaN"),
		map[string]interface{}{"nested": []interface{}{json.Number("1e9999")}},
		[]interface{}{json.Number("1e9999")},
	}
	for _, value := range values {
		if _, err := normalizeJSONNumbers(value); !errors.Is(err, Malformed) {
			t.Fatal(value, err)
		}
	}
	normalized, err := normalizeJSONNumbers([]interface{}{json.Number("1"), map[string]interface{}{"x": json.Number("-2")}})
	requireNoError(t, err)
	if !reflect.DeepEqual(normalized, []interface{}{uint64(1), map[string]interface{}{"x": int64(-2)}}) {
		t.Fatal(normalized)
	}
	for _, value := range []string{
		string([]byte{0xff}), `"trailing\`, `"\u12"`, `"\ugggg"`, `"\udc00"`,
		`"\ud800"`, `"\ud800\xabcd"`, `"\ud800\uGGGG"`, `"\ud800\u0041"`,
	} {
		if validJSONStrings(value) {
			t.Fatal("invalid JSON escape accepted", value)
		}
	}
	var decoded interface{}
	if err := strictJSONUnmarshal(`"\ud800"`, &decoded); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
}

func TestCloudInitMalformedAndNumericContracts(t *testing.T) {
	base := "CLOUD_INIT|0|event|op|" + testVM + "|" + testEvent
	for _, key := range []string{
		"CLOUD_INIT", base + "|x", "CLOUD_INIT|x|event|op|" + testEvent,
		"CLOUD_INIT|0|event||" + testEvent, "CLOUD_INIT|0|event|op|bad",
		"CLOUD_INIT|0|event|op|bad|" + testEvent,
	} {
		entry := DecodeEntries([]Record{{key, "{}"}})[0]
		if entry.Raw == nil || entry.Raw.Error != Malformed {
			t.Fatal(key, entry)
		}
	}
	if _, err := parseCloudKey("OTHER|0|event|op|" + testEvent); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
	if _, err := parseCloudKey("CLOUD_INIT|0|event|op\x00|" + testEvent); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
	if _, err := decodeCloudSingle("invalid", "{}"); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
	for _, value := range []string{
		`{"msg_i":0}`, `{"msg":1}`, `null`, `[]`, `{}`, `{} true`,
		`{"msg":"\ud800"}`, `{"extra":1e9999}`,
		`{"name":"op","type":"wrong","ts":"` + testTimestamp + `","msg":"text"}`,
		`{"name":"op","type":"event","ts":1,"msg":"text"}`,
		`{"name":"op","type":"event","ts":"invalid","msg":"text"}`,
	} {
		if _, err := decodeCloudSingle(base, value); !errors.Is(err, Malformed) {
			t.Fatalf("%s: %v", value, err)
		}
	}
	key := cloudKey{kind: "start", name: "op", eventID: testEvent}
	for _, field := range []string{"result", "duration"} {
		metadata := map[string]interface{}{"name": "op", "type": "start", "ts": testTimestamp, field: nil}
		if _, err := cloudDiagnostic(key, metadata, "text"); !errors.Is(err, Malformed) {
			t.Fatal(field, err)
		}
	}
	key.kind = "compressed"
	if _, err := cloudDiagnostic(key, map[string]interface{}{"name": "op", "type": "compressed", "ts": testTimestamp}, "invalid"); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
	key.kind = "finish"
	for _, result := range []string{"FAIL", "SUCCESS"} {
		d, err := cloudDiagnostic(key, map[string]interface{}{"name": "op", "type": "finish", "ts": testTimestamp, "result": result, "duration": uint64(1)}, "text")
		requireNoError(t, err)
		want := Error
		if result == "SUCCESS" {
			want = OK
		}
		if d.Result == nil || *d.Result != want {
			t.Fatal(d)
		}
	}
	for _, value := range []interface{}{nil, "1", int64(-1), uint64(math.MaxUint64), math.NaN(), math.Inf(1), float64(-1), float64(1e100)} {
		if _, err := cloudDuration(value); !errors.Is(err, Malformed) {
			t.Fatal(value, err)
		}
	}
	d, err := cloudDuration(int64(0))
	requireNoError(t, err)
	if d != (Duration{}) {
		t.Fatal(d)
	}
}

func TestCloudChunkAndEnvelopeFailures(t *testing.T) {
	base := "CLOUD_INIT|0|event|op|" + testEvent
	valid := `{"name":"op","type":"event","ts":"` + testTimestamp + `","msg_i":0,"msg":"one"}`
	for _, value := range []string{
		"invalid", `{"msg":1}`, `{,"msg":"text"}`, `{"msg_i":0.0,"msg":"text"}`,
		`{"msg_i":1,"msg":"text"}`, strings.Replace(valid, `"one"`, `"\x"`, 1),
	} {
		_, err := decodeCloudChunks(base, []diagnosticChunk{{index: 0, raw: RawKeyValue{Value: value}}})
		if !errors.Is(err, Malformed) {
			t.Fatalf("%s: %v", value, err)
		}
	}
	if _, err := decodeCloudChunks("invalid", nil); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
	second := strings.Replace(strings.Replace(valid, `"msg_i":0`, `"msg_i":1`, 1), `"name":"op"`, `"name":"other"`, 1)
	if _, err := decodeCloudChunks(base, []diagnosticChunk{{index: 0, raw: RawKeyValue{Value: valid}}, {index: 1, raw: RawKeyValue{Value: second}}}); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
	for _, test := range []struct {
		value string
		want  DecodeError
	}{
		{`invalid`, Malformed}, {`{}`, Malformed},
		{`{"encoding":1,"data":""}`, Malformed},
		{`{"encoding":"other","data":""}`, Undecodable},
		{`{"encoding":"gz+b64","data":1}`, Malformed},
		{`{"encoding":"gz+b64","data":"??"}`, Undecodable},
		{`{"encoding":"gz+b64","data":"AAAA"}`, Undecodable},
	} {
		if _, _, err := cloudMessage(test.value, true); !errors.Is(err, test.want) {
			t.Fatal(test.value, err)
		}
	}
	// A cloud-init group failure must preserve every original member.
	entries := DecodeEntries([]Record{{base + "|0", valid}, {base + "|1", second}})
	if len(entries) != 2 || entries[0].Raw == nil || entries[1].Raw == nil {
		t.Fatal(entries)
	}
}
