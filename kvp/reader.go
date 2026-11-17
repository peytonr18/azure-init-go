// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"sort"
	"strings"
)

type DiagnosticReader struct{ store *Store }

func NewDiagnosticReader(store *Store) *DiagnosticReader {
	return &DiagnosticReader{store: store}
}

// Entries returns decoded items in pool order. A failed group retains every
// physical record at its original position, annotated with a DecodeError.
// Framing, UTF-8 and I/O errors fail the whole read without returning entries.
func (r *DiagnosticReader) Entries() ([]Entry, error) {
	records, err := r.store.Dump()
	if err != nil {
		return nil, err
	}
	return DecodeEntries(records), nil
}

type diagnosticChunk struct {
	position int
	index    uint64
	raw      RawKeyValue
}

type positionedEntry struct {
	position int
	entry    Entry
}

// DecodeEntries interprets a snapshot without performing any I/O.
func DecodeEntries(records []Record) []Entry {
	entries := make([]positionedEntry, 0)
	groups := make(map[string][]diagnosticChunk)
	for position, record := range records {
		raw := RawKeyValue{Key: record.Key, Value: record.Value}
		prefix := strings.SplitN(raw.Key, "|", 2)[0]
		var base string
		var index *uint64
		var err error
		switch {
		case prefix == DiagnosticVersionID:
			base, index, err = splitNativeKey(raw.Key)
		case prefix == "CLOUD_INIT":
			base, index, err = splitCloudKey(raw.Key)
		case raw.Key == ProvisioningReportKey:
			report, parseErr := ParseProvisioningReport(raw.Value)
			if parseErr != nil {
				raw.Error = Malformed
				entries = append(entries, positionedEntry{position, Entry{Raw: &raw}})
			} else {
				entries = append(entries, positionedEntry{position, Entry{Report: &report}})
			}
			continue
		case strings.HasPrefix(prefix, "DIAG_V"):
			err = UnsupportedVersion
		default:
			entries = append(entries, positionedEntry{position, Entry{Raw: &raw}})
			continue
		}
		if err != nil {
			raw.Error = err.(DecodeError)
			entries = append(entries, positionedEntry{position, Entry{Raw: &raw}})
		} else if index != nil {
			groups[base] = append(groups[base], diagnosticChunk{position, *index, raw})
		} else {
			diagnostic, err := decodeCloudSingle(base, raw.Value)
			if err != nil {
				raw.Error = err.(DecodeError)
				entries = append(entries, positionedEntry{position, Entry{Raw: &raw}})
			} else {
				entries = append(entries, positionedEntry{position, Entry{Diagnostic: &diagnostic}})
			}
		}
	}
	for base, chunks := range groups {
		first := chunks[0].position
		var diagnostic Diagnostic
		var err error
		if strings.HasPrefix(base, "CLOUD_INIT|") {
			err = orderChunks(chunks)
			if err == nil {
				diagnostic, err = decodeCloudChunks(base, chunks)
			}
		} else {
			diagnostic, err = decodeNativeGroup(base, chunks)
		}
		if err == nil {
			entries = append(entries, positionedEntry{first, Entry{Diagnostic: &diagnostic}})
		} else {
			for _, chunk := range chunks {
				raw := chunk.raw
				raw.Error = err.(DecodeError)
				entries = append(entries, positionedEntry{chunk.position, Entry{Raw: &raw}})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].position < entries[j].position })
	result := make([]Entry, len(entries))
	for i := range entries {
		result[i] = entries[i].entry
	}
	return result
}

func splitNativeKey(key string) (string, *uint64, error) {
	position := strings.LastIndexByte(key, '|')
	if position < 0 {
		return "", nil, Malformed
	}
	index, err := parseUnsigned(key[position+1:])
	if err != nil {
		return "", nil, err
	}
	return key[:position], &index, nil
}

func decodeNativeGroup(base string, chunks []diagnosticChunk) (Diagnostic, error) {
	var diagnostic Diagnostic
	fields := strings.Split(base, "|")
	if len(fields) != 10 || fields[0] != DiagnosticVersionID || strings.ContainsRune(base, 0) {
		return diagnostic, Malformed
	}
	for _, i := range []int{1, 2, 4, 5, 6, 7} {
		if fields[i] == "" {
			return diagnostic, Malformed
		}
	}
	if _, err := parseUUID(fields[2]); err != nil {
		return diagnostic, Malformed
	}
	timestamp, err := parseTimestamp(fields[6])
	if err != nil {
		return diagnostic, Malformed
	}
	diagnostic.Kind = Kind(fields[3])
	diagnostic.DiagnosticKey = DiagnosticKey{Agent: fields[1], VMID: &fields[2], Name: fields[4],
		EventID: fields[5], Timestamp: timestamp, Encoding: Encoding(fields[7])}
	switch fields[8] {
	case "":
	case string(OK), string(Error):
		result := Outcome(fields[8])
		diagnostic.Result = &result
	default:
		return diagnostic, Malformed
	}
	if fields[9] != "" {
		duration, err := parseDuration(fields[9])
		if err != nil {
			return diagnostic, err
		}
		diagnostic.Duration = &duration
	}
	switch diagnostic.Kind {
	case Start:
		if diagnostic.Result != nil || diagnostic.Duration != nil {
			return diagnostic, Malformed
		}
	case Finish:
		if diagnostic.Result == nil || diagnostic.Duration == nil {
			return diagnostic, Malformed
		}
	case Event:
	default:
		return diagnostic, Malformed
	}
	if err := orderChunks(chunks); err != nil {
		return diagnostic, err
	}
	var value strings.Builder
	for _, chunk := range chunks {
		value.WriteString(chunk.raw.Value)
	}
	diagnostic.Payload, err = decodePayload(value.String(), diagnostic.Encoding)
	return diagnostic, err
}

func orderChunks(chunks []diagnosticChunk) error {
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].index < chunks[j].index })
	for i := 1; i < len(chunks); i++ {
		if chunks[i-1].index == chunks[i].index {
			return DuplicateChunk
		}
	}
	for i, chunk := range chunks {
		if chunk.index != uint64(i) {
			return IncompleteGroup
		}
	}
	return nil
}
