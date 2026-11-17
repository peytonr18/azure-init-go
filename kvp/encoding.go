// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

func encodePayload(payload Payload, encoding Encoding) (string, error) {
	var data bytes.Buffer
	err := encodePayloadTo(&data, payload, encoding)
	return data.String(), err
}

func encodePayloadTo(output io.Writer, payload Payload, encoding Encoding) error {
	var compressor io.WriteCloser
	switch encoding {
	case "", Plain:
		if !utf8.Valid(payload.data) {
			return ErrPayloadNotUTF8
		}
		if bytes.IndexByte(payload.data, 0) >= 0 {
			return ErrValueContainsNull
		}
		_, err := output.Write(payload.data)
		return err
	case ZlibBase64, GzipBase64:
	default:
		return fmt.Errorf("%w: %q", ErrEncoding, encoding)
	}
	encoded := base64.NewEncoder(base64.StdEncoding, output)
	if encoding == ZlibBase64 {
		compressor = zlib.NewWriter(encoded)
	} else {
		compressor = gzip.NewWriter(encoded)
	}
	if _, err := compressor.Write(payload.data); err != nil {
		return errors.Join(err, compressor.Close(), encoded.Close())
	}
	return errors.Join(compressor.Close(), encoded.Close())
}

func decodePayload(value string, encoding Encoding) (Payload, error) {
	switch encoding {
	case "", Plain:
		if !utf8.ValidString(value) {
			return Payload{}, Undecodable
		}
		return TextPayload(value), nil
	case ZlibBase64, GzipBase64:
		// Go's decoder ignores CR/LF even in Strict mode; native DIAG does not.
		if strings.ContainsAny(value, "\r\n") {
			return Payload{}, Undecodable
		}
		data, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil {
			return Payload{}, Undecodable
		}
		return decompress(data, encoding)
	default:
		return Payload{}, Undecodable
	}
}

func decompress(data []byte, encoding Encoding) (Payload, error) {
	source := bytes.NewReader(data)
	var reader io.ReadCloser
	var err error
	switch encoding {
	case ZlibBase64:
		reader, err = zlib.NewReader(source)
	case GzipBase64:
		var gzipReader *gzip.Reader
		gzipReader, err = gzip.NewReader(source)
		if err == nil {
			gzipReader.Multistream(false)
			reader = gzipReader
		}
	default:
		return Payload{}, Undecodable
	}
	if err != nil {
		return Payload{}, Undecodable
	}
	decoded, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || source.Len() != 0 {
		return Payload{}, Undecodable
	}
	return BytesPayload(decoded), nil
}
