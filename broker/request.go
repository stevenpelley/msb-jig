package broker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Reading requests the same way the provider does. A check is only as
// good as its agreement with the server about what a request says: every
// rule below closes a way for one request to mean one thing to the broker
// and another upstream.

// badPath reports why an escaped path can't be routed, or "". Dot
// segments (".", "..", or their %2e spellings, also inside an encoded
// segment such as a GitLab project path) would let the route that is
// checked differ from the one an upstream serves after normalizing.
func badPath(escaped string) string {
	for _, seg := range strings.Split(escaped, "/") {
		s, err := url.PathUnescape(seg)
		if err != nil {
			return "bad path encoding"
		}
		for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' }) {
			if part == "." || part == ".." {
				return "paths with . or .. segments are not accepted"
			}
		}
	}
	return ""
}

// DecodeJSON is json.Unmarshal for a body a check decides on. Go's
// decoder matches object keys to struct fields regardless of case, and
// the last match wins; a provider's matches exactly. So
// {"name":"a","Name":"b"} is "b" to the broker and "a" upstream. Any
// object holding two keys that are equal ignoring case is refused, at
// every depth.
func DecodeJSON(b []byte, v any) error {
	if err := uniqueKeys(b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func uniqueKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var walk func() error
	walk = func() error {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, _ := kt.(string)
				f := foldKey(k)
				if seen[f] {
					return fmt.Errorf("JSON key %q appears twice (ignoring case)", k)
				}
				seen[f] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token() // the closing delimiter
		return err
	}
	return walk()
}

// foldKey maps every rune to the smallest of its simple case folds, the
// equivalence encoding/json matches keys under (so "K", "k" and the
// Kelvin sign are one key).
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == utf8.RuneError {
			b.WriteRune(r)
			continue
		}
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < m {
				m = f
			}
		}
		b.WriteRune(m)
	}
	return b.String()
}

// ReadJSONBody reads a write's body for a check that decides from the
// body alone: it refuses parameters in the URL, which servers such as
// Rails and Grape merge with (or read instead of) the body, and keys that
// repeat ignoring case (DecodeJSON). An empty body returns nil. Decode
// the result with json.Unmarshal or DecodeJSON.
func (c *Call) ReadJSONBody() ([]byte, error) {
	if c.R.URL.RawQuery != "" {
		return nil, fmt.Errorf("parameters in the URL aren't accepted on this request")
	}
	b, err := c.ReadBody()
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	if err := uniqueKeys(b); err != nil {
		return nil, err
	}
	return b, nil
}
