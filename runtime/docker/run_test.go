package docker

import (
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/mount"

	"github.com/crunchloop/devcontainer/runtime"
)

func TestEnvMapToList_Sorted(t *testing.T) {
	got := envMapToList(map[string]string{
		"PATH": "/usr/bin",
		"HOME": "/root",
		"USER": "vscode",
	})
	want := []string{"HOME=/root", "PATH=/usr/bin", "USER=vscode"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envMapToList = %v, want %v", got, want)
	}
}

func TestEnvMapToList_Empty(t *testing.T) {
	if got := envMapToList(nil); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
	if got := envMapToList(map[string]string{}); got != nil {
		t.Errorf("expected nil for empty map, got %v", got)
	}
}

func TestToMobyMounts(t *testing.T) {
	in := []runtime.MountSpec{
		{Type: runtime.MountBind, Source: "/host", Target: "/container", ReadOnly: true},
		{Type: runtime.MountVolume, Source: "vol", Target: "/data", Propagation: "consistent"},
		{Type: runtime.MountTmpfs, Target: "/run"},
	}
	got := toMobyMounts(in)
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Type != mount.TypeBind || got[0].Source != "/host" || !got[0].ReadOnly {
		t.Errorf("[0] = %+v", got[0])
	}
	if got[1].Type != mount.TypeVolume || got[1].Consistency != "consistent" {
		t.Errorf("[1] = %+v", got[1])
	}
	if got[2].Type != mount.TypeTmpfs || got[2].Target != "/run" {
		t.Errorf("[2] = %+v", got[2])
	}
}

func TestIsContainerNotFound(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{simpleErr("Error response from daemon: No such container: deadbeef"), true},
		{simpleErr("no such container"), true},
		{simpleErr("connection refused"), false},
	}
	for _, tc := range cases {
		if got := isContainerNotFound(tc.err); got != tc.want {
			t.Errorf("isContainerNotFound(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestIsImageNotFound(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{simpleErr("Error response from daemon: No such image: alpine"), true},
		{simpleErr("manifest unknown"), true},
		{simpleErr("permission denied"), false},
	}
	for _, tc := range cases {
		if got := isImageNotFound(tc.err); got != tc.want {
			t.Errorf("isImageNotFound(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

type plainErr string

func (e plainErr) Error() string { return string(e) }
func simpleErr(s string) error    { return plainErr(s) }
