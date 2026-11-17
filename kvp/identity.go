// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

func parseUUID(value string) ([16]byte, error) {
	var uuid [16]byte
	if len(value) >= 9 && strings.EqualFold(value[:9], "urn:uuid:") {
		if len(value) != 45 {
			return uuid, ErrInvalidUUID
		}
		value = value[len("urn:uuid:"):]
	} else if len(value) == 38 && value[0] == '{' && value[37] == '}' {
		value = value[1:37]
	}
	if len(value) == 36 {
		if value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
			return uuid, ErrInvalidUUID
		}
		value = value[:8] + value[9:13] + value[14:18] + value[19:23] + value[24:]
	}
	if len(value) != 32 {
		return uuid, ErrInvalidUUID
	}
	if _, err := hex.Decode(uuid[:], []byte(value)); err != nil {
		return uuid, ErrInvalidUUID
	}
	return uuid, nil
}

func formatUUID(uuid [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:])
}

func randomUUID() (string, error) {
	return randomUUIDFrom(rand.Reader)
}

func randomUUIDFrom(reader io.Reader) (string, error) {
	var uuid [16]byte
	if _, err := io.ReadFull(reader, uuid[:]); err != nil {
		return "", err
	}
	uuid[6] = uuid[6]&0x0f | 0x40
	uuid[8] = uuid[8]&0x3f | 0x80
	return formatUUID(uuid), nil
}

// CurrentVMID reads DMI identity, reversing the first three UUID fields on Gen1.
// Unlike the Rust CLI's optional lookup, errors are returned to the caller.
func CurrentVMID() (string, error) {
	return currentVMID("/sys/class/dmi/id/product_uuid", "/sys/firmware/efi", "/dev/efi")
}

func currentVMID(productUUID, sysfsEFI, devEFI string) (string, error) {
	data, err := os.ReadFile(productUUID)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", ErrInvalidUTF8
	}
	value := strings.ToLower(strings.TrimSpace(string(data)))
	if value == "" {
		return "", fmt.Errorf("empty VM ID in %s", productUUID)
	}
	gen1 := true
	for _, path := range []string{sysfsEFI, devEFI} {
		_, err := os.Stat(path)
		if err == nil {
			gen1 = false
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	if !gen1 {
		return value, nil
	}
	uuid, err := parseUUID(value)
	if err != nil {
		return "", fmt.Errorf("invalid DMI VM ID %q: %w", value, err)
	}
	uuid[0], uuid[1], uuid[2], uuid[3] = uuid[3], uuid[2], uuid[1], uuid[0]
	uuid[4], uuid[5] = uuid[5], uuid[4]
	uuid[6], uuid[7] = uuid[7], uuid[6]
	return formatUUID(uuid), nil
}
