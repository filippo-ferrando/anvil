package vars

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExpand(t *testing.T) {
	env := map[string]string{"A": "a", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cases := map[string]string{
		"x${A}y":         "xay",
		"$A/b":           "a/b",
		"$$A":            "$A",
		"cost $":         "cost $",
		"${NO:-d}":       "d",
		"${EMPTY:-d}":    "d",
		"${EMPTY-d}":     "",
		"${NO-d}":        "d",
		"${A:?boom}":     "a",
		"${EMPTY?boom}":  "",
		"${A:-d}${A-d}$": "aa$",
	}
	for in, want := range cases {
		got, problems, err := expand(in, lookup)
		if err != nil || len(problems) > 0 || got != want {
			t.Errorf("expand(%q) = %q, %v, %v; want %q", in, got, problems, err, want)
		}
	}
	for in, want := range map[string]string{
		"$NO":            "NO is not set",
		"${NO}":          "NO is not set",
		"${EMPTY:?boom}": "EMPTY: boom",
		"${NO?}":         "NO: is not set",
	} {
		if _, problems, _ := expand(in, lookup); len(problems) != 1 || problems[0] != want {
			t.Errorf("expand(%q) problems = %v, want %q", in, problems, want)
		}
	}
	for _, bad := range []string{"${A", "${}", "${A:x}", "${1A}"} {
		if _, _, err := expand(bad, lookup); err == nil {
			t.Errorf("expand(%q) should fail", bad)
		}
	}
}

func TestInterpolate(t *testing.T) {
	manifest := `
vars:
  TAG: "16"
  CPUS: 1
intent: ${NAME}
members:
  db:
    image: postgres:${TAG}
    cpus: ${CPUS}
    env:
      ${KEY}: "${PASS}"
      PORT: ${PORT}
    command: ["echo", "$$HOME"]
`
	out, err := Interpolate([]byte(manifest), map[string]string{
		"NAME": "shop", "CPUS": "2", "PASS": "a: b # c", "PORT": "5432",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Intent  string
		Members map[string]struct {
			Image   string
			CPUs    int `yaml:"cpus"`
			Env     map[string]string
			Command []string
		}
	}
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	db := got.Members["db"]
	if got.Intent != "shop" || db.Image != "postgres:16" || db.CPUs != 2 {
		t.Errorf("got %+v", got)
	}
	want := map[string]string{"${KEY}": "a: b # c", "PORT": "5432"}
	if !reflect.DeepEqual(db.Env, want) || !reflect.DeepEqual(db.Command, []string{"echo", "$HOME"}) {
		t.Errorf("env %v, command %v", db.Env, db.Command)
	}

	_, err = Interpolate([]byte("a: $X\nb: ${Y:?needs Y}\nc: $X\n"), nil)
	if err == nil || err.Error() != "manifest variables: X is not set; Y: needs Y" {
		t.Errorf("err = %v", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "anvil.yaml")
	data := []byte("a: ${FROM_FLAG} ${FROM_SHELL} ${FROM_DOTENV} ${NOWHERE:-x}\n")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("# c\nexport FROM_DOTENV='d'\nFROM_SHELL=ignored\nUNUSED=u\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FROM_SHELL", "s")
	t.Setenv("FROM_FLAG", "ignored")
	got, err := Load(manifest, "", data, map[string]string{"FROM_FLAG": "f", "EXTRA": "e"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"FROM_FLAG": "f", "EXTRA": "e", "FROM_SHELL": "s", "FROM_DOTENV": "d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load = %v, want %v", got, want)
	}

	if _, err := Load(filepath.Join(t.TempDir(), "anvil.yaml"), "", data, nil); err != nil {
		t.Errorf("missing default .env should be fine: %v", err)
	}
	if _, err := Load(manifest, filepath.Join(dir, "nope"), data, nil); err == nil {
		t.Error("missing --env-file should fail")
	}
	os.WriteFile(filepath.Join(dir, "bad"), []byte("NOEQUALS\n"), 0o600)
	if _, err := Load(manifest, filepath.Join(dir, "bad"), data, nil); err == nil || !strings.Contains(err.Error(), "bad:1") {
		t.Errorf("bad env file err = %v", err)
	}
}
