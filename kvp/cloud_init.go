// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"reflect"
	"strings"
)

type cloudKey struct {
	kind    string
	name    string
	vmID    *string
	eventID string
}

func splitCloudKey(key string) (string, *uint64, error) {
	fields := strings.Split(key, "|")
	indexed := false
	switch len(fields) {
	case 5:
	case 6:
		_, err := parseUUID(fields[5])
		indexed = err != nil
	case 7:
		indexed = true
	default:
		return "", nil, Malformed
	}
	base := key
	var index *uint64
	if indexed {
		position := strings.LastIndexByte(key, '|')
		value, err := parseUnsigned(key[position+1:])
		if err != nil {
			return "", nil, err
		}
		base, index = key[:position], &value
	}
	_, err := parseCloudKey(base)
	return base, index, err
}

func parseCloudKey(base string) (cloudKey, error) {
	var key cloudKey
	fields := strings.Split(base, "|")
	if (len(fields) != 5 && len(fields) != 6) || fields[0] != "CLOUD_INIT" || strings.ContainsRune(base, 0) {
		return key, Malformed
	}
	for _, field := range fields {
		if field == "" {
			return key, Malformed
		}
	}
	for _, c := range fields[1] {
		if c < '0' || c > '9' {
			return key, Malformed
		}
	}
	key.kind, key.name, key.eventID = fields[2], fields[3], fields[len(fields)-1]
	if _, err := parseUUID(key.eventID); err != nil {
		return key, Malformed
	}
	if len(fields) == 6 {
		if _, err := parseUUID(fields[4]); err != nil {
			return key, Malformed
		}
		key.vmID = &fields[4]
	}
	return key, nil
}

func decodeCloudSingle(base, value string) (Diagnostic, error) {
	key, err := parseCloudKey(base)
	if err != nil {
		return Diagnostic{}, err
	}
	metadata, err := cloudMetadata(value)
	if err != nil {
		return Diagnostic{}, err
	}
	if _, present := metadata["msg_i"]; present {
		return Diagnostic{}, Malformed
	}
	message, ok := metadata["msg"].(string)
	if !ok {
		return Diagnostic{}, Malformed
	}
	return cloudDiagnostic(key, metadata, message)
}

func cloudMetadata(value string) (map[string]interface{}, error) {
	if !validJSONStrings(value) {
		return nil, Malformed
	}
	var metadata map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	if err := decoder.Decode(&metadata); err != nil || metadata == nil {
		return nil, Malformed
	}
	if decoder.InputOffset() < int64(len(value)) && strings.TrimSpace(value[decoder.InputOffset():]) != "" {
		return nil, Malformed
	}
	if _, err := normalizeJSONNumbers(metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

func decodeCloudChunks(base string, chunks []diagnosticChunk) (Diagnostic, error) {
	key, err := parseCloudKey(base)
	if err != nil {
		return Diagnostic{}, err
	}
	var metadata map[string]interface{}
	var escaped strings.Builder
	for _, chunk := range chunks {
		value := chunk.raw.Value
		position := strings.LastIndex(value, `,"msg":`)
		if position < 0 {
			return Diagnostic{}, Malformed
		}
		fragment := value[position+len(`,"msg":`):]
		if !strings.HasPrefix(fragment, `"`) || !strings.HasSuffix(fragment, `"}`) {
			return Diagnostic{}, Malformed
		}
		current, err := cloudMetadata(value[:position] + "}")
		if err != nil {
			return Diagnostic{}, err
		}
		index, ok := current["msg_i"].(uint64)
		if !ok || index != chunk.index {
			return Diagnostic{}, Malformed
		}
		delete(current, "msg_i")
		if metadata != nil && !reflect.DeepEqual(metadata, current) {
			return Diagnostic{}, Malformed
		}
		metadata = current
		escaped.WriteString(fragment[1 : len(fragment)-2])
	}
	var message string
	if err := strictJSONUnmarshal(`"`+escaped.String()+`"`, &message); err != nil {
		return Diagnostic{}, Malformed
	}
	return cloudDiagnostic(key, metadata, message)
}

func cloudDiagnostic(key cloudKey, metadata map[string]interface{}, message string) (Diagnostic, error) {
	var diagnostic Diagnostic
	if metadata["name"] != key.name || metadata["type"] != key.kind {
		return diagnostic, Malformed
	}
	stamp, ok := metadata["ts"].(string)
	if !ok {
		return diagnostic, Malformed
	}
	timestamp, err := parseTimestamp(stamp)
	if err != nil {
		return diagnostic, Malformed
	}
	payload, encoding, err := cloudMessage(message, key.kind == "compressed")
	if err != nil {
		return diagnostic, err
	}
	diagnostic.DiagnosticKey = DiagnosticKey{Agent: "CLOUD_INIT", VMID: key.vmID, Name: key.name,
		EventID: key.eventID, Timestamp: timestamp, Encoding: encoding}
	diagnostic.Payload = payload
	switch key.kind {
	case "start":
		if _, present := metadata["result"]; present {
			return diagnostic, Malformed
		}
		if _, present := metadata["duration"]; present {
			return diagnostic, Malformed
		}
		diagnostic.Kind = Start
	case "finish":
		var result Outcome
		switch metadata["result"] {
		case "SUCCESS":
			result = OK
		case "FAIL":
			result = Error
		default:
			return diagnostic, Malformed
		}
		duration, err := cloudDuration(metadata["duration"])
		if err != nil {
			return diagnostic, err
		}
		diagnostic.Kind, diagnostic.Result, diagnostic.Duration = Finish, &result, &duration
	default:
		diagnostic.Kind = Event
	}
	return diagnostic, nil
}

func cloudDuration(number interface{}) (Duration, error) {
	var micros uint64
	if seconds, ok := number.(uint64); ok {
		if seconds > math.MaxUint64/1_000_000 {
			return Duration{}, Malformed
		}
		micros = seconds * 1_000_000
	} else {
		var seconds float64
		switch number := number.(type) {
		case float64:
			seconds = number
		case int64:
			seconds = float64(number)
		default:
			return Duration{}, Malformed
		}
		value := seconds * 1e6
		if math.IsNaN(value) || value < 0 || value >= math.Ldexp(1, 64) {
			return Duration{}, Malformed
		}
		// Below 2^64, rounding a finite f64 cannot cross that boundary:
		// values this large are already integral, with spacing greater than one.
		micros = uint64(math.Round(value))
	}
	return Duration{Seconds: micros / 1_000_000, Nanoseconds: uint32(micros%1_000_000) * 1000}, nil
}

func cloudMessage(message string, compressed bool) (Payload, Encoding, error) {
	var envelope map[string]interface{}
	err := strictJSONUnmarshal(message, &envelope)
	_, hasEncoding := envelope["encoding"]
	_, hasData := envelope["data"]
	if !compressed && (err != nil || !hasEncoding || !hasData) {
		return TextPayload(message), Plain, nil
	}
	if err != nil {
		return Payload{}, "", Malformed
	}
	encoding, ok := envelope["encoding"].(string)
	if !ok {
		return Payload{}, "", Malformed
	}
	if encoding != string(GzipBase64) {
		return Payload{}, "", Undecodable
	}
	data, ok := envelope["data"].(string)
	if !ok {
		return Payload{}, "", Malformed
	}
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '\v' || r == '\f' {
			return -1
		}
		return r
	}, data)
	bytes, err := base64.StdEncoding.Strict().DecodeString(compact)
	if err != nil {
		return Payload{}, "", Undecodable
	}
	actualEncoding := ZlibBase64
	if len(bytes) >= 2 && bytes[0] == 0x1f && bytes[1] == 0x8b {
		actualEncoding = GzipBase64
	}
	payload, err := decompress(bytes, actualEncoding)
	return payload, GzipBase64, err
}
