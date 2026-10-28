package yamlcheck

import "testing"

func TestSyntaxLine(t *testing.T) {
	cases := map[string]int{
		"a: 1\nb: [x\nc: 2\n":         2, // yaml.v3 itself says line 1
		"a:\n  - x\n - y\n":           3,
		"a: 1\n\tb: 2\n":              2,
		"a: 1\nb: {c: 1}\n":           0,
		"#cloud-config\nruncmd: []\n": 0,
	}
	for data, want := range cases {
		p := Syntax(data)
		got := 0
		if p != nil {
			got = p.Line
		}
		if got != want {
			t.Errorf("%q: line %d, want %d (%+v)", data, got, want, p)
		}
	}
}
