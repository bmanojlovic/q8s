package server

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

// isJSONPatch reports whether the request carries an RFC 6902 JSON Patch
// (Content-Type application/json-patch+json). Terraform's Kubernetes
// provider uses JSON Patch ops to update Secret data keys, because merge
// patches cannot delete map entries.
func isJSONPatch(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct == "application/json-patch+json"
}

// applyJSONPatch applies an RFC 6902 JSON Patch (an array of ops) to doc in
// place. It supports the full op set — add, remove, replace, move, copy,
// test — over objects and arrays. Any failing op aborts the patch with an
// error; the caller decides whether to persist the partially-applied result.
func applyJSONPatch(doc map[string]interface{}, patch []map[string]interface{}) error {
	for i, op := range patch {
		typ, _ := op["op"].(string)
		path, _ := op["path"].(string)
		fail := func(err error) error {
			return fmt.Errorf("op %d (%s %q): %w", i, typ, path, err)
		}
		switch typ {
		case "add":
			segs, err := parsePath(path)
			if err != nil {
				return fail(err)
			}
			if _, err := patchSet(doc, segs, op["value"], true); err != nil {
				return fail(err)
			}
		case "replace":
			segs, err := parsePath(path)
			if err != nil {
				return fail(err)
			}
			if _, err := patchSet(doc, segs, op["value"], false); err != nil {
				return fail(err)
			}
		case "remove":
			segs, err := parsePath(path)
			if err != nil {
				return fail(err)
			}
			if _, _, err := patchRemove(doc, segs); err != nil {
				return fail(err)
			}
		case "test":
			segs, err := parsePath(path)
			if err != nil {
				return fail(err)
			}
			cur, err := patchGet(doc, segs)
			if err != nil {
				return fail(err)
			}
			if !reflect.DeepEqual(cur, op["value"]) {
				return fail(fmt.Errorf("test failed: value does not match"))
			}
		case "move":
			from, _ := op["from"].(string)
			fromSegs, err := parsePath(from)
			if err != nil {
				return fail(err)
			}
			toSegs, err := parsePath(path)
			if err != nil {
				return fail(err)
			}
			if _, removed, err := patchRemove(doc, fromSegs); err != nil {
				return fail(err)
			} else if _, err := patchSet(doc, toSegs, removed, true); err != nil {
				return fail(err)
			}
		case "copy":
			from, _ := op["from"].(string)
			fromSegs, err := parsePath(from)
			if err != nil {
				return fail(err)
			}
			toSegs, err := parsePath(path)
			if err != nil {
				return fail(err)
			}
			val, err := patchGet(doc, fromSegs)
			if err != nil {
				return fail(err)
			}
			if _, err := patchSet(doc, toSegs, val, true); err != nil {
				return fail(err)
			}
		default:
			return fail(fmt.Errorf("unsupported op %q", typ))
		}
	}
	return nil
}

// parsePath splits an RFC 6902 path into segments and unescapes ~0 (~) and
// ~1 (/). An empty path (whole document) yields no segments.
func parsePath(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path must start with /")
	}
	raw := strings.Split(path[1:], "/")
	segs := make([]string, 0, len(raw))
	for _, s := range raw {
		if strings.Contains(s, "~") {
			var b strings.Builder
			for i := 0; i < len(s); i++ {
				if s[i] != '~' {
					b.WriteByte(s[i])
					continue
				}
				if i+1 >= len(s) {
					return nil, fmt.Errorf("invalid ~ escape")
				}
				i++
				switch s[i] {
				case '0':
					b.WriteByte('~')
				case '1':
					b.WriteByte('/')
				default:
					return nil, fmt.Errorf("invalid ~ escape")
				}
			}
			s = b.String()
		}
		segs = append(segs, s)
	}
	return segs, nil
}

// patchGet returns the value at segs, or an error if the path doesn't exist.
func patchGet(cur interface{}, segs []string) (interface{}, error) {
	for _, seg := range segs {
		switch c := cur.(type) {
		case map[string]interface{}:
			v, ok := c[seg]
			if !ok {
				return nil, fmt.Errorf("no member %q", seg)
			}
			cur = v
		case []interface{}:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil, fmt.Errorf("no array element %q", seg)
			}
			cur = c[i]
		default:
			return nil, fmt.Errorf("cannot descend into %T", cur)
		}
	}
	return cur, nil
}

// patchSet sets value at segs, returning the new root so callers can assign
// the result back into the parent slot (required for array appends, whose
// backing slice may be replaced). add=true permits creating the final
// member (RFC 6902 add); add=false requires it to exist (replace).
func patchSet(cur interface{}, segs []string, value interface{}, add bool) (interface{}, error) {
	if len(segs) == 0 {
		return nil, fmt.Errorf("cannot set the whole document")
	}
	seg := segs[0]
	switch c := cur.(type) {
	case map[string]interface{}:
		if len(segs) == 1 {
			if _, ok := c[seg]; !ok && !add {
				return nil, fmt.Errorf("no member %q", seg)
			}
			c[seg] = value
			return c, nil
		}
		child, ok := c[seg]
		if !ok {
			return nil, fmt.Errorf("no member %q", seg)
		}
		newChild, err := patchSet(child, segs[1:], value, add)
		if err != nil {
			return nil, err
		}
		c[seg] = newChild
		return c, nil
	case []interface{}:
		if seg == "-" {
			if len(segs) != 1 || !add {
				return nil, fmt.Errorf("invalid use of -")
			}
			return append(c, value), nil
		}
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= len(c) {
			return nil, fmt.Errorf("no array element %q", seg)
		}
		newChild, err := patchSet(c[i], segs[1:], value, add)
		if err != nil {
			return nil, err
		}
		c[i] = newChild
		return c, nil
	default:
		return nil, fmt.Errorf("cannot descend into %T", cur)
	}
}

// patchRemove deletes the value at segs, returning the new root and the
// removed value. Like patchSet, the new root must be assigned back into the
// parent slot (array removal shortens the slice).
func patchRemove(cur interface{}, segs []string) (interface{}, interface{}, error) {
	if len(segs) == 0 {
		return nil, nil, fmt.Errorf("cannot remove the whole document")
	}
	seg := segs[0]
	switch c := cur.(type) {
	case map[string]interface{}:
		if len(segs) == 1 {
			v, ok := c[seg]
			if !ok {
				return nil, nil, fmt.Errorf("no member %q", seg)
			}
			delete(c, seg)
			return c, v, nil
		}
		child, ok := c[seg]
		if !ok {
			return nil, nil, fmt.Errorf("no member %q", seg)
		}
		newChild, removed, err := patchRemove(child, segs[1:])
		if err != nil {
			return nil, nil, err
		}
		c[seg] = newChild
		return c, removed, nil
	case []interface{}:
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= len(c) {
			return nil, nil, fmt.Errorf("no array element %q", seg)
		}
		if len(segs) == 1 {
			v := c[i]
			return append(c[:i], c[i+1:]...), v, nil
		}
		newChild, removed, err := patchRemove(c[i], segs[1:])
		if err != nil {
			return nil, nil, err
		}
		c[i] = newChild
		return c, removed, nil
	default:
		return nil, nil, fmt.Errorf("cannot descend into %T", cur)
	}
}
