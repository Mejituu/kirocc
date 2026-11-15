package customprov

import (
	"encoding/json/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestJSON5ToJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{
			name: "line comment becomes same-length spaces",
			in:   "{\"a\": 1 // note\n}",
			want: "{\"a\": 1        \n}",
		},
		{
			name: "block comment keeps newlines",
			in:   "{/* one\n   two\n   three */\"a\": 1}",
			want: "{      \n      \n           \"a\": 1}",
		},
		{
			name: "trailing comma in object",
			in:   `{"a": 1,}`,
			want: `{"a": 1 }`,
		},
		{
			name: "trailing comma in array",
			in:   `{"a": [1, 2,]}`,
			want: `{"a": [1, 2 ]}`,
		},
		{
			name: "nested trailing commas",
			in:   `{"a": [{"b": 1,},],}`,
			want: `{"a": [{"b": 1 } ] }`,
		},
		{
			name: "trailing comma separated from the closer by a comment",
			in:   `{"a": [1, /* end */ ]}`,
			want: `{"a": [1            ]}`,
		},
		{
			name: "comment after comma before line break",
			in:   "{\"a\": [1, // pair\n]}",
			want: "{\"a\": [1         \n]}",
		},
		{
			name: "unquoted keys",
			in:   `{name: 'value', $other: 1, _third: 2}`,
			want: `{"name": "value", "$other": 1, "_third": 2}`,
		},
		{
			name: "unquoted key with unicode letters",
			in:   `{名字: 1}`,
			want: `{"名字": 1}`,
		},
		{
			name: "unquoted key followed by comment and colon",
			in:   `{key /* c */ : 1}`,
			want: `{"key"         : 1}`,
		},
		{
			name: "single-quoted strings",
			in:   `{'a': 'b', 'c': 'd'}`,
			want: `{"a": "b", "c": "d"}`,
		},
		{
			name: "unescaped double quote inside single quotes",
			in:   `{'say "hi"': 'x'}`,
			want: `{"say \"hi\"": "x"}`,
		},
		{
			name: "escaped single quote",
			in:   `{'it\'s': 1}`,
			want: `{"it's": 1}`,
		},
		{
			name: "escaped double quote inside single quotes stays escaped",
			in:   `{'a': 'quote: \"'}`,
			want: `{"a": "quote: \""}`,
		},
		{
			name: "line continuation is dropped",
			in:   "{'a\\\nb': 1}",
			want: `{"ab": 1}`,
		},
		{
			name: "hex escape becomes unicode escape",
			in:   `{'a': '\x41B'}`,
			want: `{"a": "\u0041B"}`,
		},
		{
			name: "null escape becomes unicode escape",
			in:   `{'a': '\0'}`,
			want: `{"a": "\u0000"}`,
		},
		{
			name: "vertical tab escape becomes unicode escape",
			in:   `{'a': '\v'}`,
			want: `{"a": "\u000b"}`,
		},
		{
			name: "json escapes pass through",
			in:   `{'a': 'tab\tnl\nquote\"backslash\\slash\/'}`,
			want: `{"a": "tab\tnl\nquote\"backslash\\slash\/"}`,
		},
		{
			name: "hexadecimal number",
			in:   `{"n": 0x1F}`,
			want: `{"n": 31}`,
		},
		{
			name: "negative hexadecimal number",
			in:   `{"n": -0x1F}`,
			want: `{"n": -31}`,
		},
		{
			name: "leading plus is dropped",
			in:   `{"n": +5}`,
			want: `{"n": 5}`,
		},
		{
			name: "leading decimal point gains a zero",
			in:   `{"n": .5}`,
			want: `{"n": 0.5}`,
		},
		{
			name: "negative leading decimal point",
			in:   `{"n": -.5}`,
			want: `{"n": -0.5}`,
		},
		{
			name: "trailing decimal point gains a zero",
			in:   `{"n": 5.}`,
			want: `{"n": 5.0}`,
		},
		{
			name: "decimal point before an exponent gains a zero",
			in:   `{"n": 5.e3}`,
			want: `{"n": 5.0e3}`,
		},
		{
			name: "negative decimal point before an exponent",
			in:   `{"n": -5.e3}`,
			want: `{"n": -5.0e3}`,
		},
		{
			name: "double-quoted hex escape becomes unicode escape",
			in:   `{"a": "\x41"}`,
			want: `{"a": "\u0041"}`,
		},
		{
			name: "double-quoted null and vertical tab escapes",
			in:   `{"a": "\0", "b": "\v"}`,
			want: `{"a": "\u0000", "b": "\u000b"}`,
		},
		{
			name: "double-quoted line continuation is dropped",
			in:   "{\"a\": \"x\\\ny\"}",
			want: `{"a": "xy"}`,
		},
		{
			name:    "double-quoted unknown escape is rejected",
			in:      `{"a": "\a"}`,
			wantErr: `unknown escape \a`,
		},
		{
			name:    "double-quoted unterminated string is rejected",
			in:      `{"a": "abc`,
			wantErr: "unterminated string",
		},
		{
			name: "comment markers inside double-quoted strings are content",
			in:   `{"a": "// not a comment", "b": "/* also not */"}`,
			want: `{"a": "// not a comment", "b": "/* also not */"}`,
		},
		{
			name: "single quote inside double-quoted string is content",
			in:   `{"a": "it's fine"}`,
			want: `{"a": "it's fine"}`,
		},
		{
			name: "true false null pass through",
			in:   `{a: true, b: false, c: null}`,
			want: `{"a": true, "b": false, "c": null}`,
		},
		{
			name:    "Infinity is rejected",
			in:      `{"n": Infinity}`,
			wantErr: "Infinity cannot be represented in standard JSON",
		},
		{
			name:    "negative Infinity is rejected",
			in:      `{"n": -Infinity}`,
			wantErr: "Infinity cannot be represented in standard JSON",
		},
		{
			name:    "NaN is rejected",
			in:      `{"n": NaN}`,
			wantErr: "NaN cannot be represented in standard JSON",
		},
		{
			name:    "hexadecimal overflow is rejected",
			in:      `{"n": 0xFFFFFFFFFFFFFFFFF}`,
			wantErr: "invalid hexadecimal number",
		},
		{
			name:    "unknown escape is rejected",
			in:      `{'a': '\a'}`,
			wantErr: `unknown escape \a`,
		},
		{
			name:    "bad hex escape is rejected",
			in:      `{'a': '\xZZ'}`,
			wantErr: "invalid \\x escape",
		},
		{
			name:    "unterminated string is rejected",
			in:      `{'a': 'b}`,
			wantErr: "unterminated string",
		},
		{
			name:    "unterminated block comment is rejected",
			in:      `{"a": 1 /* never closed`,
			wantErr: "unterminated block comment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := json5ToJSON([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("output %q, want error containing %q", string(out), tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(out) != tt.want {
				t.Errorf("output = %q, want %q", string(out), tt.want)
			}
		})
	}
}

func TestJSON5ToJSON_ErrorPosition(t *testing.T) {
	// The unknown escape's backslash sits on line 4, column 9 (1-based).
	in := "{\n  'a': 1,\n  'b': 2,\n  'c': '\\a'\n}"
	_, err := json5ToJSON([]byte(in))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "line 4, column 9:") {
		t.Errorf("error should carry line 4 column 9, got: %v", err)
	}
}

func TestJSON5ToJSON_DecodeErrorLineAfterBlockComment(t *testing.T) {
	// The bare word is invalid JSON; the decoder error's offset must map to
	// the same line as in the original file, which only holds because block
	// comments become same-length spaces.
	src := "{\n  /* a\n     b\n     c */ 'a': notaword\n}"
	converted, err := json5ToJSON([]byte(src))
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
	if len(converted) != len(src) {
		t.Fatalf("converted length = %d, want %d (same-length comment blanking)", len(converted), len(src))
	}
	var v any
	decodeErr := json.Unmarshal(converted, &v)
	if decodeErr == nil {
		t.Fatal("expected a decode error for the bare word")
	}
	msg := decodeErr.Error()
	idx := strings.Index(msg, "offset ")
	if idx < 0 {
		t.Fatalf("decode error lacks an offset: %v", decodeErr)
	}
	num := msg[idx+len("offset "):]
	if comma := strings.IndexAny(num, " )"); comma >= 0 {
		num = num[:comma]
	}
	offset, err := strconv.Atoi(num)
	if err != nil {
		t.Fatalf("parse offset from %q: %v", msg, err)
	}
	// The decoder points at the bare word (off-by-one inside it at most);
	// either way the offset must map to the original file's line 4.
	if offset < strings.Index(src, "notaword") || offset > strings.Index(src, "notaword")+len("notaword") {
		t.Fatalf("error offset %d, want it inside %q", offset, "notaword")
	}
	line := 1 + strings.Count(src[:offset], "\n")
	if line != 4 {
		t.Fatalf("error line = %d, want 4", line)
	}
}

func TestJSON5ToJSON_PlainJSONRoundTrip(t *testing.T) {
	src := `{"providers":{"bailian":{"base_url":"http://p","auth_token":"t","models":["a[1m]","b"]}},"kiro":{"mappings":[{"model":"m","to":{"model":"x"}}]}}`
	converted, err := json5ToJSON([]byte(src))
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
	var direct, via any
	if err := json.Unmarshal([]byte(src), &direct); err != nil {
		t.Fatalf("direct decode: %v", err)
	}
	if err := json.Unmarshal(converted, &via); err != nil {
		t.Fatalf("converted decode: %v", err)
	}
	if !reflect.DeepEqual(direct, via) {
		t.Errorf("decoded values differ:\n%#v\n%#v", direct, via)
	}
	if string(src) != string(converted) {
		t.Errorf("plain JSON should convert to itself:\nsrc: %s\nout: %s", src, string(converted))
	}
}
