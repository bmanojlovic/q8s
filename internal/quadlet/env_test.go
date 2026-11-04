package quadlet

import (
	"strings"
	"testing"
)

func TestEnvLine(t *testing.T) {
	cases := []struct {
		name    string
		varName string
		value   string
		want    string
	}{
		{name: "plain", varName: "FOO", value: "bar", want: "Environment=FOO=bar\n"},
		{name: "empty", varName: "EMPTY", value: "", want: "Environment=EMPTY=\n"},
		{
			name:    "spaces",
			varName: "STRATA_SET",
			value:   "conversation-cache-mib=65536 conversation-cache-slots=24 prompt-cache=12",
			want:    `Environment="STRATA_SET=conversation-cache-mib=65536 conversation-cache-slots=24 prompt-cache=12"` + "\n",
		},
		{name: "double quote", varName: "MSG", value: `say "hi"`, want: `Environment="MSG=say \"hi\""` + "\n"},
		{name: "backslash", varName: "WINPATH", value: `C:\tmp\x`, want: `Environment="WINPATH=C:\\tmp\\x"` + "\n"},
		{name: "percent", varName: "PCT", value: "100%", want: `Environment="PCT=100%%"` + "\n"},
		{
			name:    "all specials",
			varName: "MIXED",
			value:   `a b\c"d%e`,
			want:    `Environment="MIXED=a b\\c\"d%%e"` + "\n",
		},
		{name: "equals in value", varName: "ARGS", value: "a=b c=d", want: `Environment="ARGS=a=b c=d"` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := envLine(tc.varName, tc.value)
			if err != nil {
				t.Fatalf("envLine(%q, %q): %v", tc.varName, tc.value, err)
			}
			if got != tc.want {
				t.Errorf("envLine(%q, %q) = %q, want %q", tc.varName, tc.value, got, tc.want)
			}
		})
	}
}

func TestEnvLineRejectsControlCharacters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "newline", value: "one\ntwo"},
		{name: "carriage return", value: "one\rtwo"},
		{name: "tab", value: "one\ttwo"},
		{name: "NUL", value: "one\x00two"},
		{name: "DEL", value: "one\x7ftwo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := envLine("BAD_VAR", tc.value)
			if err == nil {
				t.Fatal("expected an error for a control character in the value")
			}
			if !strings.Contains(err.Error(), "BAD_VAR") {
				t.Errorf("error %q should name the variable", err)
			}
			if !strings.Contains(err.Error(), "control character") {
				t.Errorf("error %q should mention control characters", err)
			}
		})
	}
}

// TestEnvLineRoundTrip checks the rendered line with systemd's parsing rules:
// quoted words are grouped, \" and \\ are unescaped, and %% is a literal %.
func TestEnvLineRoundTrip(t *testing.T) {
	for _, value := range []string{
		"",
		"plain",
		"with space",
		" leading and trailing ",
		`C:\tmp\x`,
		`say "hi"`,
		"100%",
		`a b\c"d%e`,
		"x=y=z",
		"%s %d %%",
		`\`,
		`"`,
		`%`,
	} {
		line, err := envLine("VAR", value)
		if err != nil {
			t.Fatalf("envLine(VAR, %q): %v", value, err)
		}
		name, got := parseSystemdEnvLine(t, line)
		if name != "VAR" {
			t.Errorf("parsed name = %q, want VAR (line %q)", name, line)
		}
		if got != value {
			t.Errorf("parsed value = %q, want %q (line %q)", got, value, line)
		}
	}
}

// parseSystemdEnvLine parses one rendered Environment= line the way systemd's
// unit-file parser does for the subset envLine emits: whitespace separates
// words, double quotes group, backslash escapes the next byte inside quotes,
// and %% is a literal percent. It returns the assignment's name and value.
func parseSystemdEnvLine(t *testing.T, line string) (string, string) {
	t.Helper()
	const prefix = "Environment="
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "\n") {
		t.Fatalf("malformed Environment= line %q", line)
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(line, prefix), "\n")

	var words []string
	var cur strings.Builder
	inQuote := false
	hasToken := false
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			hasToken = true
		case c == '\\' && inQuote:
			i++
			if i >= len(rest) {
				t.Fatalf("dangling backslash in %q", line)
			}
			cur.WriteByte(rest[i])
			hasToken = true
		case c == '%':
			if i+1 >= len(rest) || rest[i+1] != '%' {
				t.Fatalf("single %% specifier in %q", line)
			}
			cur.WriteByte('%')
			i++
			hasToken = true
		case !inQuote && (c == ' ' || c == '\t'):
			if hasToken {
				words = append(words, cur.String())
				cur.Reset()
				hasToken = false
			}
		default:
			cur.WriteByte(c)
			hasToken = true
		}
	}
	if inQuote {
		t.Fatalf("unterminated quote in %q", line)
	}
	if hasToken {
		words = append(words, cur.String())
	}
	if len(words) != 1 {
		t.Fatalf("expected one Environment= assignment, got %d: %q", len(words), words)
	}
	name, value, ok := strings.Cut(words[0], "=")
	if !ok {
		t.Fatalf("Environment= word has no '=': %q", words[0])
	}
	return name, value
}
