// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

func strictJSONUnmarshal(value string, target interface{}) error {
	if !validJSONStrings(value) {
		return Malformed
	}
	return json.Unmarshal([]byte(value), target)
}

// serde_json keeps unsigned integers, signed integers, and floats distinct.
// Normalize once so chunk metadata equality and numeric bounds match Rust.
func normalizeJSONNumbers(value interface{}) (interface{}, error) {
	switch value := value.(type) {
	case json.Number:
		text := value.String()
		if !strings.ContainsAny(text, ".eE") {
			if unsigned, err := strconv.ParseUint(text, 10, 64); err == nil {
				return unsigned, nil
			}
			if text != "-0" {
				if signed, err := strconv.ParseInt(text, 10, 64); err == nil {
					return signed, nil
				}
			}
		}
		number, err := value.Float64()
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return nil, Malformed
		}
		return number, nil
	case map[string]interface{}:
		for key, child := range value {
			normalized, err := normalizeJSONNumbers(child)
			if err != nil {
				return nil, err
			}
			value[key] = normalized
		}
	case []interface{}:
		for i, child := range value {
			normalized, err := normalizeJSONNumbers(child)
			if err != nil {
				return nil, err
			}
			value[i] = normalized
		}
	}
	return value, nil
}

// encoding/json replaces unpaired UTF-16 surrogates, while serde_json rejects
// them. Check string escapes before letting Go decode the JSON syntax.
func validJSONStrings(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	quoted := false
	for i := 0; i < len(value); i++ {
		if value[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || value[i] != '\\' {
			continue
		}
		i++
		if i >= len(value) {
			return false
		}
		if value[i] != 'u' {
			continue
		}
		if i+4 >= len(value) {
			return false
		}
		code, err := strconv.ParseUint(value[i+1:i+5], 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code < 0xd800 || code > 0xdbff {
			continue
		}
		if i+6 >= len(value) || value[i+1:i+3] != `\u` {
			return false
		}
		low, err := strconv.ParseUint(value[i+3:i+7], 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
