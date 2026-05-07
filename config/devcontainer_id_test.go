package config

import (
	"regexp"
	"testing"
)

func TestDevcontainerID_Stable(t *testing.T) {
	a := DevcontainerID("/home/u/proj", ".devcontainer/devcontainer.json")
	b := DevcontainerID("/home/u/proj", ".devcontainer/devcontainer.json")
	if a != b {
		t.Fatalf("expected stable id across calls, got %q vs %q", a, b)
	}
}

func TestDevcontainerID_DifferentInputsDiffer(t *testing.T) {
	cases := []struct {
		name string
		a, b [2]string
	}{
		{
			name: "different workspace folders",
			a:    [2]string{"/a", "x.json"},
			b:    [2]string{"/b", "x.json"},
		},
		{
			name: "different config paths",
			a:    [2]string{"/a", "x.json"},
			b:    [2]string{"/a", "y.json"},
		},
		{
			name: "null-byte separator prevents concatenation collision",
			// Without the separator: "/a/b" + "" == "/a" + "/b". The
			// separator forces these into distinct hash inputs.
			a: [2]string{"/a/b", ""},
			b: [2]string{"/a", "/b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ida := DevcontainerID(tc.a[0], tc.a[1])
			idb := DevcontainerID(tc.b[0], tc.b[1])
			if ida == idb {
				t.Errorf("expected different ids for %v vs %v, both = %q", tc.a, tc.b, ida)
			}
		})
	}
}

var lowercaseBase32 = regexp.MustCompile(`^[a-z2-7]{16}$`)

func TestDevcontainerID_Format(t *testing.T) {
	cases := []struct {
		local, config string
	}{
		{"/some/path", ".devcontainer/devcontainer.json"},
		{"", ""},
		{"/", "/"},
		{"with spaces", "weird path/file.json"},
		{"/üñîçødé", ".dc/devcontainer.json"},
	}
	for _, tc := range cases {
		id := DevcontainerID(tc.local, tc.config)
		if len(id) != 16 {
			t.Errorf("DevcontainerID(%q,%q): expected 16 chars, got %d (%q)",
				tc.local, tc.config, len(id), id)
			continue
		}
		if !lowercaseBase32.MatchString(id) {
			t.Errorf("DevcontainerID(%q,%q) = %q is not lowercase base32",
				tc.local, tc.config, id)
		}
	}
}
