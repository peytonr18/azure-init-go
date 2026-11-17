//go:build !linux

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import "os"

func lockFile(*os.File, bool) error { return ErrUnsupportedOS }
