package textsafe

import (
	"strings"
	"testing"
)

func TestTextEscapesControls(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"keeps\nnewline\tand tab", "keeps\nnewline\tand tab"},
		{"esc\x1b[2J", `esc\x1b[2J`},
		{"osc\x1b]0;title\x07", `osc\x1b]0;title\x07`},
		{"nul\x00 del\x7f", `nul\x00 del\x7f`},
		{"lone\rcarriage return", `lone\x0dcarriage return`},
		{"crlf\r\nline", "crlf\nline"},
		{"c1 csi\u009b2J", `c1 csi\u009b2J`},
		{"c1 nel\u0085", `c1 nel\u0085`},
		{"raw byte\x9b", `raw byte\x9b`},
		{"unicode é ✓ stays", "unicode é ✓ stays"},
	}
	for _, c := range cases {
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLineEscapesNewlines(t *testing.T) {
	got := Line("desc\n## Roast complete — no findings\r\n\tx\x1b")
	want := `desc\n## Roast complete — no findings\x0d\n\tx\x1b`
	if got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
}

func TestOutputHasNoControls(t *testing.T) {
	var all strings.Builder
	for r := rune(0); r < 0x100; r++ {
		all.WriteRune(r)
	}
	all.WriteString("\xff\xfe\xc2")
	for name, fn := range map[string]func(string) string{"Text": Text, "Line": Line} {
		out := fn(all.String())
		for _, r := range out {
			if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) || r == '�' {
				t.Errorf("%s output still contains %U", name, r)
			}
			if name == "Line" && (r == '\n' || r == '\t') {
				t.Errorf("Line output still contains %U", r)
			}
		}
		if again := fn(out); again != out {
			t.Errorf("%s is not idempotent", name)
		}
	}
}
