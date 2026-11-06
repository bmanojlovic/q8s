package quadlet

import (
	"fmt"
	"strings"
	"unicode"
)

// envLine renders one systemd Environment= line. Values are left unquoted
// when that is safe, so existing units keep byte-identical output (and their
// template hash stays the same); a value containing whitespace, a double
// quote, a backslash or a percent sign is emitted as one double-quoted word
// with the characters systemd would otherwise interpret escaped. A control
// character cannot be represented in a one-line unit directive, so it is an
// error that names the variable.
func envLine(name, value string) (string, error) {
	return unitAssignmentLine("Environment", name, value, false)
}

// unitAssignmentLine renders one "Directive=name=value" unit-file line
// shared by Environment= and Annotation=. Unquoted values keep the historic
// Environment= output byte-identical; whitespace, a double quote, a
// backslash or a percent sign forces one double-quoted word with those
// characters escaped.
//
// escapeControls selects the annotation rules: control characters are legal
// in a Kubernetes annotation value, so they force quoting and are written as
// C escapes inside the quotes. With escapeControls false (Environment=) a
// control character stays an error naming the variable, as before.
func unitAssignmentLine(directive, name, value string, escapeControls bool) (string, error) {
	if !escapeControls {
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return "", fmt.Errorf("environment variable %q value must not contain control characters (found %q)", name, r)
			}
		}
	}

	needsQuoting := strings.IndexFunc(value, func(r rune) bool {
		if escapeControls && (r < 0x20 || r == 0x7f) {
			return true
		}
		return unicode.IsSpace(r) || r == '"' || r == '\\' || r == '%'
	}) >= 0
	if !needsQuoting {
		return fmt.Sprintf("%s=%s=%s\n", directive, name, value), nil
	}

	var b strings.Builder
	b.WriteString(directive)
	b.WriteString(`="`)
	b.WriteString(name)
	b.WriteByte('=')
	for _, r := range value {
		// systemd expands %-specifiers in unit files, so a literal percent
		// must be doubled; backslashes and quotes are C-escaped inside the
		// double quotes.
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '%':
			b.WriteByte('%')
			b.WriteRune(r)
		case escapeControls && (r < 0x20 || r == 0x7f):
			// A control character would end or split the directive line if
			// written raw, so keep it inside the quotes as a C escape.
			switch r {
			case '\n':
				b.WriteString(`\n`)
			case '\t':
				b.WriteString(`\t`)
			case '\r':
				b.WriteString(`\r`)
			default:
				fmt.Fprintf(&b, `\x%02x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString("\"\n")
	return b.String(), nil
}

// EnvLine is the exported form of envLine, for other systemd unit-file
// writers such as the installer's q8s-api.service.
func EnvLine(name, value string) (string, error) {
	return envLine(name, value)
}
