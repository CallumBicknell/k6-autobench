package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestWorkloadScriptUsesNoModernSyntax is a lint with a specific failure behind it.
//
// The script had its options folded together with object spread, which k6 v2.1.0
// compiles and k6 v0.50.0 rejects outright: "SyntaxError: Unexpected token". The
// whole run failed on a machine that had been running autobench's older script
// fine, and no test said so, because the tests ran the k6 installed on the
// development machine.
//
// k6 embeds different JavaScript engines across its versions, so the script is
// held to the syntax all of them accept. Each entry here is a construct one of the
// supported versions rejects, with the version that found it.
func TestWorkloadScriptUsesNoModernSyntax(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "k6", "runner.js"))
	if err != nil {
		t.Fatalf("read the workload script: %v", err)
	}
	code := stripJSComments(raw)

	banned := []struct {
		pattern string
		why     string
	}{
		{`...`, "object or array spread, which k6 v0.50.0's compiler rejects"},
		{`?.`, "optional chaining, which older k6 engines do not compile"},
		{`??`, "nullish coalescing, which older k6 engines do not compile"},
		{`replaceAll(`, "String.replaceAll, which older k6 engines do not have"},
		{`Object.fromEntries(`, "Object.fromEntries, which older k6 engines do not have"},
	}

	for _, b := range banned {
		if idx := bytes.Index(code, []byte(b.pattern)); idx >= 0 {
			line := bytes.Count(code[:idx], []byte("\n")) + 1
			t.Errorf("k6/runner.js:%d uses %s (%s)", line, b.pattern, b.why)
		}
	}
}

// stripJSComments removes // and /* */ comments, so the lint reads code rather
// than the prose describing it. It is not a parser: a comment marker inside a
// string literal would confuse it, and the script has none.
func stripJSComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inLine, inBlock := false, false
	for i := 0; i < len(src); i++ {
		switch {
		case inLine:
			if src[i] == '\n' {
				inLine = false
				out = append(out, src[i])
			} else {
				out = append(out, ' ')
			}
		case inBlock:
			if src[i] == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				out = append(out, ' ', ' ')
				i++
			} else if src[i] == '\n' {
				out = append(out, src[i])
			} else {
				out = append(out, ' ')
			}
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine = true
			out = append(out, ' ', ' ')
			i++
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			out = append(out, ' ', ' ')
			i++
		default:
			out = append(out, src[i])
		}
	}
	return out
}
