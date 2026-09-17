package quadlet

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// This file validates every client-supplied string field before it's
// interpolated into a generated quadlet/systemd unit file. These files are
// plain "Key=Value" INI-style text — an unvalidated value containing a
// newline doesn't just corrupt a line, it opens a brand new line (and, with
// a blank line plus "[Section]", a brand new section) in a file that systemd
// will load and execute directives from. Rejecting bad input here, at the
// one place all four generator functions funnel through, closes that off
// regardless of which handler (Pod, Job, CronJob, Deployment instance,
// startup reconciliation) constructed the spec.

// imageRE is a conservative superset of valid OCI/Docker image references
// (registry/repo:tag@digest) — permissive enough for real image names,
// strict enough to exclude whitespace, quotes, and control characters.
var imageRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._\-/:@]*$`)

func validateImage(image string) error {
	if image == "" || len(image) > 512 || !imageRE.MatchString(image) {
		return fmt.Errorf("image %q is not a valid image reference", image)
	}
	return nil
}

// envNameRE matches POSIX environment variable name rules (same as real k8s).
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateEnvName(name string) error {
	if !envNameRE.MatchString(name) {
		return fmt.Errorf("environment variable name %q is invalid", name)
	}
	return nil
}

// nameRE matches the RFC 1123 label rules q8s enforces on Namespace/Name at
// the API boundary (see internal/server/validate.go). Used here for values
// that reference other q8s-managed objects by name — PVC claims, ConfigMap/
// Secret names in a volume mount — so a malformed reference can't smuggle
// extra directives into the generated unit file either.
var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validateRefName(kind, value string) error {
	if value == "" || len(value) > 253 || !nameRE.MatchString(value) {
		return fmt.Errorf("%s %q is invalid", kind, value)
	}
	return nil
}

// pathRE allows normal absolute path characters, excluding control
// characters and double quotes (which could break out of quoted values).
var pathRE = regexp.MustCompile(`^/[^\x00-\x1f"]*$`)

func validatePath(kind, value string) error {
	if !pathRE.MatchString(value) {
		return fmt.Errorf("%s %q must be an absolute path with no control characters", kind, value)
	}
	return nil
}

// noControlChars rejects newlines, carriage returns, and NUL bytes — the
// characters that let a value break out of a single "Key=Value" line.
// Used for fields (command/arg elements, env values) that are otherwise
// meant to be free-form.
func noControlChars(kind, value string) error {
	for _, r := range value {
		if r == '\n' || r == '\r' || r == 0 {
			return fmt.Errorf("%s must not contain control characters", kind)
		}
	}
	return nil
}

// labelKeyRE matches a k8s label key: an optional DNS-subdomain prefix
// ("example.com/") followed by a name segment of alphanumerics, '-', '_' or
// '.'. Loose compared to the full k8s spec (doesn't bound segment lengths),
// but excludes '=', whitespace and control characters — the shapes that
// would corrupt or redefine the "Label=key=value" line it's written into.
var labelKeyRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]*[a-z0-9])?/)?[a-zA-Z0-9]([-a-zA-Z0-9._]*[a-zA-Z0-9])?$`)

func validateLabelPair(key, value string) error {
	if key == "" || len(key) > 253 || !labelKeyRE.MatchString(key) {
		return fmt.Errorf("label key %q is invalid", key)
	}
	if err := noControlChars("label value", value); err != nil {
		return err
	}
	return nil
}

// cronFieldRE allows the standard cron field syntax: digits, '*', ranges,
// lists, and steps.
var cronFieldRE = regexp.MustCompile(`^[0-9*/,-]+$`)

// cronFieldBounds holds the min/max value of each of the five cron fields
// (minute, hour, day-of-month, month, day-of-week). Day-of-week allows both
// 0 and 7 for Sunday, as cron does.
var cronFieldBounds = [5][2]int{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // day of month
	{1, 12}, // month
	{0, 7},  // day of week (0 and 7 = Sunday)
}

func validateCronSchedule(schedule string) error {
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return fmt.Errorf("schedule %q must have exactly 5 fields", schedule)
	}
	for i, f := range fields {
		if !cronFieldRE.MatchString(f) {
			return fmt.Errorf("schedule %q has an invalid field %q", schedule, f)
		}
		// Reject out-of-range values up front ("99 25 * * *" is not a valid
		// cron schedule) instead of letting them surface later as an opaque
		// systemd timer load error at daemon-reload time — after the
		// CronJob has already been accepted and stored.
		min, max := cronFieldBounds[i][0], cronFieldBounds[i][1]
		if _, err := expandCronField(f, min, max); err != nil {
			return fmt.Errorf("schedule %q: field %d: %v", schedule, i+1, err)
		}
	}
	return nil
}

// expandCronField returns the set of integers a cron field matches, bounded
// by min and max. Supports "*", single values ("5"), ranges ("1-5"), steps
// ("*/2", "3/5" meaning 3,8,13…, "1-15/2"), and comma-separated lists of
// those. Sortedness is not guaranteed, but every value is within [min,max]
// and unique within its list element.
func expandCronField(f string, min, max int) ([]int, error) {
	if f == "" {
		return nil, fmt.Errorf("empty field")
	}
	var out []int
	for _, part := range strings.Split(f, ",") {
		step := 1
		hasStep := false
		if i := strings.IndexByte(part, '/'); i >= 0 {
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s < 1 {
				return nil, fmt.Errorf("invalid step %q", part)
			}
			step, hasStep = s, true
			part = part[:i]
		}
		lo, hi := min, max
		if part == "*" {
			// full range as-is
		} else if bounds := strings.SplitN(part, "-", 2); len(bounds) == 2 {
			var err error
			if lo, err = strconv.Atoi(bounds[0]); err != nil {
				return nil, fmt.Errorf("invalid range %q", part)
			}
			if hi, err = strconv.Atoi(bounds[1]); err != nil {
				return nil, fmt.Errorf("invalid range %q", part)
			}
		} else {
			v, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid value %q", part)
			}
			lo, hi = v, v
			// A bare value with a step ("3/5") runs from that value to the
			// end of the field's range in cron — not just the one value.
			if hasStep {
				hi = max
			}
		}
		if lo < min || hi > max {
			return nil, fmt.Errorf("value %q outside range %d-%d", part, min, max)
		}
		if lo > hi {
			return nil, fmt.Errorf("descending range %q", part)
		}
		for v := lo; v <= hi; v += step {
			out = append(out, v)
		}
	}
	return out, nil
}
