// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package joinscript

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// MaxScriptSize bounds what is handed to the parser. The parser recurses once per nesting
// level, and a stack overflow is fatal in Go, so deep enough input would kill the process.
const MaxScriptSize = 64 << 10

// Hash fingerprints a script, so that two of them can be compared without keeping either.
func Hash(script string) string {
	sum := sha256.Sum256([]byte(script))

	return hex.EncodeToString(sum[:])
}

// Validate parses a rendered join script and reports what is wrong with it, so one that
// cannot run is rejected here rather than on a DPU. The name is what errors call the script.
func Validate(script, name string) error {
	// A ConfigMap holds a megabyte, and around 150k of nesting is enough to overflow. The
	// real script is a couple of kilobytes, so this is generous and still well clear.
	if len(script) > MaxScriptSize {
		return fmt.Errorf("join script is %d bytes, over the %d byte limit", len(script), MaxScriptSize)
	}

	// LangBash matches what DPF runs the script with. In POSIX mode arrays, here strings and
	// process substitution would all be rejected.
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))

	// The tree is thrown away. Printing it back out would rewrite backticks and respace
	// array subscripts, changing what the DPU runs as root.
	if _, err := parser.Parse(strings.NewReader(script), name); err != nil {
		// A syntax.ParseError already names the script, the line and the column.
		return fmt.Errorf("join script is not valid bash: %w", err)
	}

	return nil
}
