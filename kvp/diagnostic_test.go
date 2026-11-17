// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const testEvent = "11111111-2222-4333-8444-555555555555"

func testWriter(t *testing.T, options ...WriterOption) (*DiagnosticWriter, *Store) {
	t.Helper()
	store := testStore(t, Safe)
	writer, err := NewDiagnosticWriter(store, testAgent, testVM, options...)
	requireNoError(t, err)
	writer.now = func() time.Time { return time.Date(2026, 6, 17, 1, 2, 3, 123456789, time.UTC) }
	writer.newID = func() (string, error) { return testEvent, nil }
	return writer, store
}

func nativeKey(kind, encoding, result, duration, index string) string {
	return strings.Join([]string{"DIAG", testAgent, testVM, kind, "operation", testEvent,
		testTimestamp, encoding, result, duration, index}, "|")
}

func TestDiagnosticWireAndRoundTrip(t *testing.T) {
	writer, store := testWriter(t)
	requireNoError(t, writer.EmitStart(testEvent, "operation", TextPayload("start"), Plain))
	requireNoError(t, writer.EmitFinish(testEvent, "operation", TextPayload("finish"), Plain, OK,
		Duration{Seconds: 1, Nanoseconds: 123456789}))
	requireNoError(t, writer.EmitEvent("observation", TextPayload("event"), Plain, nil, nil))
	records, err := store.Dump()
	requireNoError(t, err)
	wantStart := "DIAG|" + testAgent + "|" + testVM + "|start|operation|" + testEvent + "|2026-06-17T01:02:03.123Z|none|||0"
	if records[0].Key != wantStart || !strings.Contains(records[1].Key, "|ok|1.123456|0") {
		t.Fatal(records)
	}
	entries, err := NewDiagnosticReader(store).Entries()
	requireNoError(t, err)
	if len(entries) != 3 {
		t.Fatal(entries)
	}
	for i, kind := range []Kind{Start, Finish, Event} {
		if entries[i].Diagnostic == nil || entries[i].Diagnostic.Kind != kind {
			t.Fatalf("entry %d: %#v", i, entries[i])
		}
	}
	finish := entries[1].Diagnostic
	if *finish.Result != OK || *finish.Duration != (Duration{Seconds: 1, Nanoseconds: 123456000}) {
		t.Fatal(finish)
	}
	if finish.Timestamp.Nanosecond() != 123000000 || finish.EventID != testEvent {
		t.Fatal(finish)
	}
}

func TestEncodingsAndStrictStreamCompletion(t *testing.T) {
	for _, encoding := range []Encoding{Plain, ZlibBase64, GzipBase64} {
		for _, data := range [][]byte{nil, []byte("UTF-8 \u00e9"), bytes.Repeat([]byte("repeat"), 500)} {
			value, err := encodePayload(BytesPayload(data), encoding)
			requireNoError(t, err)
			payload, err := decodePayload(value, encoding)
			requireNoError(t, err)
			if !bytes.Equal(payload.Bytes(), data) || payload.IsBinary() != (encoding != Plain) {
				t.Fatal("payload type/content changed")
			}
			if encoding == Plain {
				continue
			}
			compressed, err := base64.StdEncoding.DecodeString(value)
			requireNoError(t, err)
			for _, invalid := range [][]byte{
				compressed[:len(compressed)-1],
				append(append([]byte(nil), compressed...), 0),
				append(append([]byte(nil), compressed...), compressed...),
				[]byte("not compressed"),
			} {
				_, err := decompress(invalid, encoding)
				if !errors.Is(err, Undecodable) {
					t.Fatalf("accepted invalid %s stream: %v", encoding, err)
				}
			}
			if _, err := decodePayload(value[:4]+"\n"+value[4:], encoding); !errors.Is(err, Undecodable) {
				t.Fatal("native base64 must not tolerate whitespace")
			}
			other := ZlibBase64
			if encoding == ZlibBase64 {
				other = GzipBase64
			}
			if _, err := decodePayload(value, other); !errors.Is(err, Undecodable) {
				t.Fatal("native encoding tokens are not interchangeable")
			}
		}
	}
	for _, test := range []struct {
		payload  Payload
		encoding Encoding
		want     error
	}{
		{BytesPayload([]byte{0xff}), Plain, ErrPayloadNotUTF8},
		{TextPayload("nul\x00byte"), Plain, ErrValueContainsNull},
		{TextPayload("text"), "unsupported", ErrEncoding},
	} {
		if _, err := encodePayload(test.payload, test.encoding); !errors.Is(err, test.want) {
			t.Fatal(err)
		}
	}
	value, err := encodePayload(BytesPayload([]byte{0xff, 0x00}), ZlibBase64)
	requireNoError(t, err)
	payload, err := decodePayload(value, ZlibBase64)
	requireNoError(t, err)
	if !bytes.Equal(payload.Bytes(), []byte{0xff, 0x00}) {
		t.Fatal(payload)
	}
}

func TestChunkingPreservesUTF8AndEmptyPayload(t *testing.T) {
	for _, message := range []string{"", strings.Repeat("a", MaxChunkBytes), strings.Repeat("\u754c", 1000)} {
		writer, store := testWriter(t)
		requireNoError(t, writer.EmitEvent("long", TextPayload(message), Plain, nil, nil))
		records, err := store.Dump()
		requireNoError(t, err)
		for i, record := range records {
			if len(record.Value) > MaxChunkBytes || !utf8.ValidString(record.Value) || !strings.HasSuffix(record.Key, "|"+strconv.Itoa(i)) {
				t.Fatalf("invalid chunk: %#v", record)
			}
		}
		// Physical order and adjacency are not grouping requirements.
		for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
			records[i], records[j] = records[j], records[i]
		}
		entries := DecodeEntries(records)
		if len(entries) != 1 || entries[0].Diagnostic == nil {
			t.Fatal(entries)
		}
		text, ok := entries[0].Diagnostic.Payload.Text()
		if !ok || text != message {
			t.Fatal("failed to reassemble payload")
		}
	}
}

func TestWriterValidationWritesNothing(t *testing.T) {
	writer, store := testWriter(t)
	for _, test := range []struct {
		name string
		emit func() error
		want error
	}{
		{"empty-name", func() error { return writer.EmitStart(testEvent, "", TextPayload(""), Plain) }, ErrEmptyField},
		{"delimiter", func() error { return writer.EmitStart(testEvent, "a|b", TextPayload(""), Plain) }, ErrFieldDelimiter},
		{"name-bytes", func() error { return writer.EmitStart(testEvent, strings.Repeat("\u00e9", 33), TextPayload(""), Plain) }, ErrFieldTooLong},
		{"event-id", func() error { return writer.EmitStart("invalid", "op", TextPayload(""), Plain) }, ErrInvalidUUID},
		{"nul", func() error { return writer.EmitStart(testEvent, "op", TextPayload("\x00"), Plain) }, ErrValueContainsNull},
		{"duration", func() error {
			return writer.EmitFinish(testEvent, "op", TextPayload(""), Plain, OK, Duration{Nanoseconds: 1e9})
		}, ErrInvalidDuration},
		{"encoding", func() error { return writer.EmitStart(testEvent, "op", TextPayload(""), "other") }, ErrEncoding},
		{"chunks", func() error {
			return writer.EmitStart(testEvent, "op", TextPayload(strings.Repeat("a", MaxChunks*MaxChunkBytes+1)), Plain)
		}, ErrTooManyChunks},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.emit(); !errors.Is(err, test.want) {
				t.Fatalf("%v, want %v", err, test.want)
			}
			if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
				t.Fatalf("validation created a pool: %v", err)
			}
		})
	}
	for _, test := range []struct{ agent, vmID string }{
		{"", testVM}, {"a|b", testVM}, {strings.Repeat("a", 33), testVM},
		{"a", ""}, {"a", "not-a-uuid"}, {"a", strings.Repeat("0", 37)},
	} {
		if _, err := NewDiagnosticWriter(store, test.agent, test.vmID); err == nil {
			t.Fatal("accepted invalid producer")
		}
	}
	writer.newID = func() (string, error) { return "", fmt.Errorf("entropy unavailable") }
	if err := writer.EmitEvent("op", TextPayload(""), Plain, nil, nil); err == nil {
		t.Fatal("entropy failure was hidden")
	}
}

func TestReaderErrorsAndPhysicalOrdering(t *testing.T) {
	good := Record{nativeKey("event", "none", "", "", "0"), "hello"}
	for _, test := range []struct {
		name    string
		records []Record
		want    DecodeError
	}{
		{"gap", []Record{{nativeKey("event", "none", "", "", "1"), "hello"}}, IncompleteGroup},
		{"duplicate", []Record{good, good}, DuplicateChunk},
		{"bad-index", []Record{{nativeKey("event", "none", "", "", "+0"), ""}}, Malformed},
		{"index-overflow", []Record{{nativeKey("event", "none", "", "", "18446744073709551616"), ""}}, Malformed},
		{"version", []Record{{"DIAG_V2|anything", "keep"}}, UnsupportedVersion},
		{"kind", []Record{{nativeKey("other", "none", "", "", "0"), ""}}, Malformed},
		{"start-result", []Record{{nativeKey("start", "none", "ok", "", "0"), ""}}, Malformed},
		{"finish-missing", []Record{{nativeKey("finish", "none", "ok", "", "0"), ""}}, Malformed},
		{"negative-duration", []Record{{nativeKey("finish", "none", "ok", "-1", "0"), ""}}, Malformed},
		{"unknown-result", []Record{{nativeKey("finish", "none", "warn", "1", "0"), ""}}, Malformed},
		{"unknown-encoding", []Record{{nativeKey("event", "future", "", "", "0"), ""}}, Undecodable},
		{"bad-payload", []Record{{nativeKey("event", "gz+b64", "", "", "0"), "invalid"}}, Undecodable},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, entry := range DecodeEntries(test.records) {
				if entry.Raw == nil || entry.Raw.Error != test.want {
					t.Fatalf("got %#v, want %s", entry, test.want)
				}
			}
		})
	}
	first := Record{nativeKey("event", "none", "", "", "1"), "world"}
	last := Record{nativeKey("event", "none", "", "", "0"), "hello "}
	entries := DecodeEntries([]Record{first, {"ordinary", "keep"}, last})
	if len(entries) != 2 || entries[0].Diagnostic == nil || entries[1].Raw == nil {
		t.Fatal(entries)
	}
	if string(entries[0].Diagnostic.Payload.Bytes()) != "hello world" {
		t.Fatal("incorrect reassembly")
	}
	entries = DecodeEntries([]Record{first, {"ordinary", "keep"}, first})
	if len(entries) != 3 || entries[0].Raw.Error != DuplicateChunk || entries[1].Raw.Error != "" || entries[2].Raw.Error != DuplicateChunk {
		t.Fatal("failed chunks did not retain physical positions")
	}
}

func TestIndependentEventFieldsAndLargeDurations(t *testing.T) {
	for _, result := range []string{"", "ok", "error"} {
		for _, duration := range []string{"", "0", "0.000000001", "18446744073709551615.999999999", "1.25e1"} {
			entries := DecodeEntries([]Record{{nativeKey("event", "none", result, duration, "0"), "v"}})
			if len(entries) != 1 || entries[0].Diagnostic == nil {
				t.Fatalf("result=%q duration=%q: %#v", result, duration, entries)
			}
			diagnostic := entries[0].Diagnostic
			if (diagnostic.Result == nil) != (result == "") || (diagnostic.Duration == nil) != (duration == "") {
				t.Fatal(diagnostic)
			}
		}
	}
	for _, invalid := range []string{"NaN", "inf", "-0.1", "18446744073709551616", "1e100", "0x1p0", "1_0", " 1"} {
		if _, err := parseDuration(invalid); !errors.Is(err, Malformed) {
			t.Fatalf("accepted %q: %v", invalid, err)
		}
	}
	duration := Duration{Seconds: math.MaxUint64, Nanoseconds: 999999999}
	if _, err := duration.TimeDuration(); !errors.Is(err, ErrInvalidDuration) {
		t.Fatal("overflow was not detected")
	}
	for precision, want := range map[Precision]string{Seconds: "18446744073709551615", Milliseconds: "18446744073709551615.999", Microseconds: "18446744073709551615.999999", Nanoseconds: "18446744073709551615.999999999"} {
		got, err := duration.Format(precision)
		requireNoError(t, err)
		if got != want {
			t.Fatal(got)
		}
	}
}

func TestRFC3339Compatibility(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"2026-08-31T12:34:56Z", "2026-08-31T12:34:56Z"},
		{"2026-08-31t12:34:56.789z", "2026-08-31T12:34:56.789Z"},
		{"2026-08-31 12:34:56Z", "2026-08-31T12:34:56Z"},
		{"2026-08-31T14:34:56.789+02:00", "2026-08-31T12:34:56.789Z"},
		{"2026-08-31T12:34:56.7891234567Z", "2026-08-31T12:34:56.789123456Z"},
		{"2026-08-31T12:34:56.78Z", "2026-08-31T12:34:56.780Z"},
		{"2026-08-31T12:34:56.789000Z", "2026-08-31T12:34:56.789Z"},
		{"2026-08-31T12:34:60.123400+00:30", "2026-08-31T12:04:60.123400Z"},
		{"0000-01-01T00:00:60+01:00", "-0001-12-31T23:00:60Z"},
		{"9999-12-31T23:59:59-01:00", "+10000-01-01T00:59:59Z"},
		{"0000-01-01T00:00:00+00:00", "0000-01-01T00:00:00Z"},
	} {
		timestamp, err := parseTimestamp(test.input)
		requireNoError(t, err)
		if timestamp.String() != test.want {
			t.Fatalf("%q -> %q, want %q", test.input, timestamp.String(), test.want)
		}
	}
	for _, invalid := range []string{
		"2026-08-31T1:34:56Z", "2026-08-31T12:34:56,123Z",
		"2026-08-31T12:34:56+24:00", "2026-08-31T12:34:56+01:60",
		"2026-13-31T12:34:56Z", "2026-08-31T12:34:61Z", "2026-08-31T12:34:56",
	} {
		if _, err := parseTimestamp(invalid); !errors.Is(err, Malformed) {
			t.Fatal("accepted", invalid, err)
		}
	}
}

func TestFloatDurationUsesExactBinaryRounding(t *testing.T) {
	for _, test := range []struct {
		input string
		want  Duration
	}{
		{"0.0000000005", Duration{Nanoseconds: 1}},
		{"0.0000000015", Duration{Nanoseconds: 1}},
		{"0.0000000025", Duration{Nanoseconds: 3}},
		{"1e-10", Duration{}},
		{"1.25e1", Duration{Seconds: 12, Nanoseconds: 500000000}},
	} {
		got, err := parseDuration(test.input)
		requireNoError(t, err)
		if got != test.want {
			t.Fatalf("%s: %#v, want %#v", test.input, got, test.want)
		}
	}
}

func TestWriterPrecisionAndCompleteKeyBudget(t *testing.T) {
	for _, test := range []struct {
		precision           Precision
		timestamp, duration string
	}{
		{Seconds, "2026-06-17T01:02:03Z", "12"},
		{Milliseconds, "2026-06-17T01:02:03.123Z", "12.123"},
		{Microseconds, "2026-06-17T01:02:03.123456Z", "12.123456"},
		{Nanoseconds, "2026-06-17T01:02:03.123456789Z", "12.123456789"},
	} {
		precision := test.precision
		writer, store := testWriter(t, WithTimestampPrecision(precision), WithDurationPrecision(precision))
		duration := Duration{Seconds: 12, Nanoseconds: 123456789}
		requireNoError(t, writer.EmitFinish(testEvent, "op", TextPayload("v"), Plain, Error, duration))
		records, err := store.Dump()
		requireNoError(t, err)
		fields := strings.Split(records[0].Key, "|")
		if fields[9] != test.duration || fields[6] != test.timestamp {
			t.Fatal(records)
		}
	}
	writer, store := testWriter(t, WithMaxNameBytes(256))
	if err := writer.EmitStart(testEvent, strings.Repeat("x", 150), TextPayload("v"), Plain); !errors.Is(err, ErrKeyTooLarge) {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatal("oversized complete key wrote data")
	}
	for _, option := range []WriterOption{WithMaxNameBytes(-1), WithTimestampPrecision(2), WithDurationPrecision(5), nil} {
		if _, err := NewDiagnosticWriter(store, testAgent, testVM, option); err == nil {
			t.Fatal("accepted invalid option")
		}
	}
}

func TestCheckedGoDurationConversions(t *testing.T) {
	for _, value := range []time.Duration{0, time.Nanosecond, 1500 * time.Millisecond, time.Duration(math.MaxInt64)} {
		duration, err := DurationFromTime(value)
		requireNoError(t, err)
		again, err := duration.TimeDuration()
		requireNoError(t, err)
		if again != value {
			t.Fatalf("%v changed to %v", value, again)
		}
	}
	if _, err := DurationFromTime(-time.Nanosecond); !errors.Is(err, ErrInvalidDuration) {
		t.Fatal(err)
	}
	for _, duration := range []Duration{
		{Seconds: 9223372036, Nanoseconds: 854775808},
		{Nanoseconds: 1000000000},
		{Seconds: math.MaxUint64},
	} {
		if _, err := duration.TimeDuration(); !errors.Is(err, ErrInvalidDuration) {
			t.Fatal("duration overflow was not rejected", err)
		}
	}
}

func TestStrictCloudJSONSurrogates(t *testing.T) {
	for _, test := range []struct {
		message string
		valid   bool
	}{
		{`"\ud83d\ude00"`, true}, {`"\ud800"`, false}, {`"\udc00"`, false},
		{`"\ud800\u0041"`, false}, {`"\\ud800"`, true}, {`"ordinary"`, true},
	} {
		value := `{"name":"op","type":"event","ts":"` + testTimestamp + `","msg":` + test.message + `}`
		record := Record{"CLOUD_INIT|0|event|op|" + testVM + "|" + testEvent, value}
		entry := DecodeEntries([]Record{record})[0]
		if (entry.Diagnostic != nil) != test.valid {
			t.Fatalf("%s: %#v", test.message, entry)
		}
	}
}

func cloudRecord(kind, message string) Record {
	data, _ := json.Marshal(map[string]interface{}{"name": "op", "type": kind, "ts": testTimestamp, "msg": message})
	return Record{"CLOUD_INIT|0|" + kind + "|op|" + testVM + "|" + testEvent, string(data)}
}

func TestCloudInitCurrentLegacyAndCompressed(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, kind := range []string{"start", "event", "log"} {
			record := cloudRecord(kind, "text")
			if legacy {
				record.Key = strings.Replace(record.Key, "|"+testVM, "", 1)
			}
			entries := DecodeEntries([]Record{record})
			if len(entries) != 1 || entries[0].Diagnostic == nil {
				t.Fatal(entries)
			}
			if (entries[0].Diagnostic.VMID == nil) != legacy {
				t.Fatal("incorrect legacy VM identity")
			}
		}
	}
	for _, encoding := range []Encoding{ZlibBase64, GzipBase64} {
		value, err := encodePayload(BytesPayload([]byte{0xff, 0x00, 0x01}), encoding)
		requireNoError(t, err)
		envelope := fmt.Sprintf(`{"encoding":"gz+b64","data":%q}`, value[:4]+" \t\n"+value[4:])
		record := cloudRecord("compressed", envelope)
		entries := DecodeEntries([]Record{record})
		if entries[0].Diagnostic == nil || !bytes.Equal(entries[0].Diagnostic.Payload.Bytes(), []byte{0xff, 0x00, 0x01}) {
			t.Fatalf("cloud-init %s: %#v", encoding, entries)
		}
		// Native gzip must still reject zlib even though cloud-init accepts it.
	}
	record := cloudRecord("finish", "finished")
	record.Value = strings.TrimSuffix(record.Value, "}") + `,"result":"SUCCESS","duration":0.1234567}`
	entries := DecodeEntries([]Record{record})
	if entries[0].Diagnostic == nil || *entries[0].Diagnostic.Duration != (Duration{Nanoseconds: 123457000}) {
		t.Fatal(entries)
	}
	record.Value = strings.Replace(record.Value, "SUCCESS", "WARN", 1)
	if entry := DecodeEntries([]Record{record})[0]; entry.Raw == nil || entry.Raw.Error != Malformed {
		t.Fatal("WARN must remain raw")
	}
}

func TestCloudChunksReassembleBeforeJSONUnescaping(t *testing.T) {
	base := "CLOUD_INIT|0|event|op|" + testVM + "|" + testEvent
	first := `{"name":"op","type":"event","ts":"` + testTimestamp + `","msg_i":0,"msg":"line\"}`
	second := `{"name":"op","type":"event","ts":"` + testTimestamp + `","msg_i":1,"msg":"nnext"}`
	entries := DecodeEntries([]Record{{base + "|1", second}, {base + "|0", first}})
	if len(entries) != 1 || entries[0].Diagnostic == nil || string(entries[0].Diagnostic.Payload.Bytes()) != "line\nnext" {
		t.Fatalf("split JSON escape did not reassemble: %#v", entries)
	}
	second = strings.Replace(second, `"msg_i":1`, `"msg_i":0`, 1)
	entries = DecodeEntries([]Record{{base + "|0", first}, {base + "|1", second}})
	if len(entries) != 2 || entries[0].Raw.Error != Malformed || entries[1].Raw.Error != Malformed {
		t.Fatal("mismatched msg_i must invalidate the entire group")
	}
}

func TestDiagnosticJSONShapes(t *testing.T) {
	writer, store := testWriter(t)
	requireNoError(t, writer.EmitEvent("binary", BytesPayload([]byte{0xff}), ZlibBase64, nil, nil))
	entries, err := NewDiagnosticReader(store).Entries()
	requireNoError(t, err)
	data, err := json.Marshal(entries)
	requireNoError(t, err)
	var got []map[string]interface{}
	requireNoError(t, json.Unmarshal(data, &got))
	if got[0]["type"] != "diagnostic" || got[0]["kind"] != "event" || got[0]["encoding"] != "zlib+b64" {
		t.Fatal(string(data))
	}
	if !reflect.DeepEqual(got[0]["payload"], map[string]interface{}{"type": "bytes", "encoding": "base64", "data": "/w=="}) {
		t.Fatal(string(data))
	}
	if _, ok := got[0]["duration"]; ok {
		t.Fatal("absent duration must not become zero")
	}
}

func TestVMIdentityLookup(t *testing.T) {
	dir := t.TempDir()
	path, sysEFI, devEFI := filepath.Join(dir, "uuid"), filepath.Join(dir, "efi"), filepath.Join(dir, "dev-efi")
	requireNoError(t, os.WriteFile(path, []byte("00112233-4455-6677-8899-AABBCCDDEEFF\n"), 0600))
	got, err := currentVMID(path, sysEFI, devEFI)
	requireNoError(t, err)
	if got != "33221100-5544-7766-8899-aabbccddeeff" {
		t.Fatal(got)
	}
	requireNoError(t, os.Mkdir(sysEFI, 0700))
	got, err = currentVMID(path, sysEFI, devEFI)
	requireNoError(t, err)
	if got != "00112233-4455-6677-8899-aabbccddeeff" {
		t.Fatal(got)
	}
	for _, value := range []string{"", "\n"} {
		requireNoError(t, os.WriteFile(path, []byte(value), 0600))
		if _, err := currentVMID(path, sysEFI, devEFI); err == nil {
			t.Fatal("missing identity error")
		}
	}
}

func FuzzDecodeEntries(f *testing.F) {
	f.Add(nativeKey("event", "none", "", "", "0"), "hello", "ordinary", "data")
	f.Add("CLOUD_INIT|0|event|name|bad", "{}", "DIAG_V2|x", "")
	f.Fuzz(func(t *testing.T, key, value, key2, value2 string) {
		records := []Record{{key, value}, {key2, value2}}
		before := append([]Record(nil), records...)
		entries := DecodeEntries(records)
		if len(entries) > len(records) || !reflect.DeepEqual(before, records) {
			t.Fatal("reader expanded or mutated physical records")
		}
		for _, entry := range entries {
			if entry.Raw != nil && (entry.Raw.Key != key || entry.Raw.Value != value) &&
				(entry.Raw.Key != key2 || entry.Raw.Value != value2) {
				t.Fatal("reader did not preserve a raw record")
			}
		}
	})
}

func TestUUIDReaderForms(t *testing.T) {
	for _, value := range []string{
		"00112233445566778899aabbccddeeff",
		"00112233-4455-6677-8899-AABBCCDDEEFF",
		"{00112233-4455-6677-8899-aabbccddeeff}",
		"urn:uuid:00112233-4455-6677-8899-aabbccddeeff",
		"URN:UUID:00112233-4455-6677-8899-aabbccddeeff",
	} {
		if _, err := parseUUID(value); err != nil {
			t.Fatalf("%s: %v", value, err)
		}
	}
	if _, err := parseUUID("urn:uuid:00112233445566778899aabbccddeeff"); err == nil {
		t.Fatal("URN requires hyphenated UUID form")
	}
}
