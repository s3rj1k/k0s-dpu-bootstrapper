// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package joinscript_test

import (
	"strings"
	"testing"

	"code.local/k0s-dpu-bootstrapper/internal/joinscript"
)

const testName = "k0s-join@1"

func TestValidate(t *testing.T) {
	script := "#!/usr/bin/env bash\nset -euo pipefail\nif [ -x /usr/local/bin/k0s ]; then\n  k0s start\nfi\n"

	if err := joinscript.Validate(script, testName); err != nil {
		t.Errorf("joinscript.Validate: %v", err)
	}
}

func TestValidateRejectsInvalidShell(t *testing.T) {
	// The whole point. This one only fails on the DPU otherwise, where the agent retries it
	// every 30s and says nothing the host cluster can see.
	err := joinscript.Validate("set -euo pipefail\nif true; then\n  echo hi\n", testName)
	if err == nil {
		t.Fatal("expected an error, got none")
	}

	// A ParseError carries name, line and column, which is what makes the report useful.
	for _, want := range []string{testName, ":2:1:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidateRejectsUnbalancedQuote(t *testing.T) {
	// What a value carrying an odd quote does to the script it is substituted into.
	if err := joinscript.Validate(`k0s install worker --labels "dpu=true`, testName); err == nil {
		t.Fatal("expected an error, got none")
	}
}

func TestValidateAcceptsBashOnlySyntax(t *testing.T) {
	// The parser has to be in bash mode. In POSIX mode all of these are rejected, and since
	// the check is fail closed that would stop a working script from ever reaching a DPU.
	for _, script := range []string{
		`echo "${K0S_URL//https/http}"`,
		`args=(--token-file /etc/k0s/join.token)`,
		`[[ -x /usr/local/bin/k0s ]] && echo yes`,
		`grep -q dpu <<<"$NODE_NAME"`,
		`diff <(echo a) <(echo b) || true`,
		"function join { k0s start; }",
	} {
		if err := joinscript.Validate(script, testName); err != nil {
			t.Errorf("bash syntax was rejected: %v\n%s", err, script)
		}
	}
}

func TestValidateAcceptsHeredocs(t *testing.T) {
	// Every step of the shipped example writes a file this way, so a heredoc the parser
	// could not follow would take the whole template down.
	script := "cat >/tmp/x <<'EOF'\n[Service]\n    ExecStart=/bin/sleep infinity\nEOF\n" +
		"cat >/tmp/y <<-TABBED\n\tindented body\n\tTABBED\n"

	if err := joinscript.Validate(script, testName); err != nil {
		t.Errorf("joinscript.Validate: %v", err)
	}
}

func TestValidateRejectsOversizedScript(t *testing.T) {
	// The parser recurses per nesting level and a Go stack overflow is fatal, so an
	// oversized template would take the whole controller down rather than fail one DPU.
	err := joinscript.Validate(strings.Repeat("(", joinscript.MaxScriptSize+1), testName)
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Errorf("error = %v, want the size limit", err)
	}

	// Deep, but under the limit, so it has to come back as an ordinary parse error.
	err = joinscript.Validate(strings.Repeat("(", joinscript.MaxScriptSize-1), testName)
	if err == nil || !strings.Contains(err.Error(), "not valid bash") {
		t.Errorf("error = %v, want a parse failure", err)
	}
}

func TestHash(t *testing.T) {
	script := "#!/usr/bin/env bash\nk0s start\n"
	again := "#!/usr/bin/env bash\n" + "k0s start\n"

	if joinscript.Hash(script) != joinscript.Hash(again) {
		t.Error("the same script hashed differently twice")
	}
	if joinscript.Hash(script) == joinscript.Hash(script+"\n") {
		t.Error("a changed script hashed the same")
	}
	if len(joinscript.Hash("")) != 64 {
		t.Errorf("Hash(\"\") = %q, want a sha256 in hex", joinscript.Hash(""))
	}
}
