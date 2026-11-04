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
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("environment variable %q value must not contain control characters (found %q)", name, r)
		}
	}

	needsQuoting := strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || r == '"' || r == '\\' || r == '%'
	}) >= 0
	if !needsQuoting {
		return fmt.Sprintf("Environment=%s=%s\n", name, value), nil
	}

	var b strings.Builder
	b.WriteString(`Environment="`)
	b.WriteString(name)
	b.WriteByte('=')
	for _, r := range value {
		// systemd expands %-specifiers in unit files, so a literal percent
		// must be doubled; backslashes and quotes are C-escaped inside the
		// double quotes.
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '%':
			b.WriteByte('%')
			b.WriteRune(r)
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
