// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"errors"
	"strings"
	"testing"
)

func TestMalformedMetadataErrorPropagation(t *testing.T) {
	base := "CLOUD_INIT|0|event|op|" + testEvent
	for _, value := range []string{
		`{"msg_i":0,"msg":1}`,
		`{"bad":,"msg":"text"}`,
	} {
		if _, err := decodeCloudChunks(base, []diagnosticChunk{{raw: RawKeyValue{Value: value}}}); !errors.Is(err, Malformed) {
			t.Fatal(value, err)
		}
	}
	key := cloudKey{kind: "finish", name: "op", eventID: testEvent}
	if _, err := cloudDiagnostic(key, map[string]interface{}{
		"name": "op", "type": "finish", "ts": testTimestamp, "result": "SUCCESS", "duration": int64(-1),
	}, "text"); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
	var text string
	if err := strictJSONUnmarshal(`"\u0041"`, &text); err != nil || text != "A" {
		t.Fatal(text, err)
	}
	if _, err := parseUnsigned(""); !errors.Is(err, Malformed) {
		t.Fatal(err)
	}
	record := Record{Key: strings.Replace(nativeKey("event", "none", "", "", "0"), testTimestamp, "invalid", 1), Value: "text"}
	entry := DecodeEntries([]Record{record})[0]
	if entry.Raw == nil || entry.Raw.Error != Malformed {
		t.Fatal(entry)
	}
}
