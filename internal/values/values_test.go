package values

import (
	"os"
	"path/filepath"
	"testing"

	"helm.sh/helm/v3/pkg/cli"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}
	return p
}

func TestMergePrecedence(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, dir, "base.yaml", "image:\n  tag: from-file\nreplicas: 1\n")
	over := writeFile(t, dir, "over.yaml", "replicas: 2\n")
	secret := writeFile(t, dir, "secret.txt", "s3cr3t")

	settings := cli.New()
	vals, err := Merge(Options{
		ValueFiles:   []string{base, over},
		Values:       []string{"image.tag=from-set", "enabled=true"},
		StringValues: []string{"port=8080"},
		JSONValues:   []string{"limits={\"cpu\":\"100m\"}"},
		FileValues:   []string{"token=" + secret},
	}, settings)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Later -f files win over earlier ones.
	if got := vals["replicas"]; got != float64(2) && got != 2 {
		t.Fatalf("replicas = %#v, want 2", got)
	}
	// --set wins over -f.
	image, ok := vals["image"].(map[string]interface{})
	if !ok {
		t.Fatalf("image = %#v, want a map", vals["image"])
	}
	if image["tag"] != "from-set" {
		t.Fatalf("image.tag = %#v, want from-set", image["tag"])
	}
	// --set parses typed scalars, --set-string keeps strings.
	if vals["enabled"] != true {
		t.Fatalf("enabled = %#v, want true", vals["enabled"])
	}
	if vals["port"] != "8080" {
		t.Fatalf("port = %#v, want the string 8080", vals["port"])
	}
	// --set-json parses structured values.
	limits, ok := vals["limits"].(map[string]interface{})
	if !ok || limits["cpu"] != "100m" {
		t.Fatalf("limits = %#v, want {cpu: 100m}", vals["limits"])
	}
	// --set-file reads the file content.
	if vals["token"] != "s3cr3t" {
		t.Fatalf("token = %#v, want s3cr3t", vals["token"])
	}
}

func TestMergeEmpty(t *testing.T) {
	vals, err := Merge(Options{}, cli.New())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(vals) != 0 {
		t.Fatalf("expected an empty map, got %#v", vals)
	}
}

func TestMergeInvalidSet(t *testing.T) {
	if _, err := Merge(Options{Values: []string{"a[=1"}}, cli.New()); err == nil {
		t.Fatal("expected an error for a malformed --set")
	}
}

func TestMergeMissingFile(t *testing.T) {
	if _, err := Merge(Options{ValueFiles: []string{filepath.Join(t.TempDir(), "nope.yaml")}}, cli.New()); err == nil {
		t.Fatal("expected an error for a missing values file")
	}
}

func TestTopLevelKeys(t *testing.T) {
	keys := TopLevelKeys(map[string]interface{}{"z": 1, "a": 2, "m": 3})
	want := []string{"a", "m", "z"}
	if len(keys) != len(want) {
		t.Fatalf("got %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("got %v, want %v", keys, want)
		}
	}
	if len(TopLevelKeys(nil)) != 0 {
		t.Fatal("nil map should yield no keys")
	}
}
