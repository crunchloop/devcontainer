package docker

import (
	"reflect"
	"testing"

	"github.com/crunchloop/devcontainer/runtime"
)

func TestComposeArgs_FilesInOrder(t *testing.T) {
	got := composeArgs("dc-abc", []string{"a.yml", "b.yml"})
	want := []string{"compose", "--project-name", "dc-abc", "-f", "a.yml", "-f", "b.yml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestComposeArgs_NoProjectName(t *testing.T) {
	// Empty project name should not emit --project-name (we never want
	// to send an empty value; compose would interpret that oddly).
	got := composeArgs("", []string{"a.yml"})
	want := []string{"compose", "-f", "a.yml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestComposeArgs_ReturnsFreshSlice(t *testing.T) {
	// The leading args feed into per-command builders that append.
	// Confirm two appends to the result of a shared call don't alias.
	a := composeArgs("p", []string{"f.yml"})
	a1 := append(a, "extra-a")
	a2 := append(a, "extra-b")
	if a1[len(a1)-1] == a2[len(a2)-1] {
		t.Errorf("composeArgs returned aliasing slice: a1=%v a2=%v", a1, a2)
	}
}

func TestBuildUpArgs(t *testing.T) {
	got := buildUpArgs(runtime.ComposeUpSpec{
		Files:       []string{"compose.yml", "dc-build.yaml", "dc-run.yaml"},
		ProjectName: "dc-w0rkspace",
		Services:    []string{"app", "db"},
	})
	want := []string{
		"compose", "--project-name", "dc-w0rkspace",
		"-f", "compose.yml",
		"-f", "dc-build.yaml",
		"-f", "dc-run.yaml",
		"up", "-d",
		"app", "db",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestBuildUpArgs_NoNoBuildFlag(t *testing.T) {
	// We deliberately omit --no-build per design §2.1: compose's
	// default skip-build-if-image-exists is right when our override
	// pins the primary service's image to a tag we built upstream.
	got := buildUpArgs(runtime.ComposeUpSpec{
		Files:       []string{"compose.yml"},
		ProjectName: "dc-x",
	})
	for _, a := range got {
		if a == "--no-build" {
			t.Errorf("buildUpArgs should not pass --no-build; got %v", got)
		}
	}
}

func TestBuildUpArgs_NoServicesUpsAll(t *testing.T) {
	got := buildUpArgs(runtime.ComposeUpSpec{
		Files:       []string{"compose.yml"},
		ProjectName: "dc-x",
	})
	want := []string{"compose", "--project-name", "dc-x", "-f", "compose.yml", "up", "-d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestBuildUpArgs_NoRecreate exercises the resume path: when the
// caller knows a container already exists for this workspace, pass
// --no-recreate so compose doesn't destroy it on spurious drift.
// Matches upstream devcontainers/cli's gate
// (`container || expectExistingContainer`).
func TestBuildUpArgs_NoRecreate(t *testing.T) {
	got := buildUpArgs(runtime.ComposeUpSpec{
		Files:       []string{"compose.yml"},
		ProjectName: "dc-x",
		Services:    []string{"app"},
		NoRecreate:  true,
	})
	want := []string{
		"compose", "--project-name", "dc-x",
		"-f", "compose.yml",
		"up", "-d", "--no-recreate",
		"app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestBuildUpArgs_NoRecreateOmittedByDefault(t *testing.T) {
	// Cold-start path: no existing container, so --no-recreate must
	// not appear (it would block first creation under some compose
	// behaviors / be misleading in flow logs).
	got := buildUpArgs(runtime.ComposeUpSpec{
		Files:       []string{"compose.yml"},
		ProjectName: "dc-x",
	})
	for _, a := range got {
		if a == "--no-recreate" {
			t.Errorf("buildUpArgs should not pass --no-recreate when NoRecreate=false; got %v", got)
		}
	}
}

func TestBuildDownArgs_Plain(t *testing.T) {
	got := buildDownArgs(runtime.ComposeDownSpec{
		Files:       []string{"compose.yml"},
		ProjectName: "dc-x",
	})
	want := []string{"compose", "--project-name", "dc-x", "-f", "compose.yml", "down"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildDownArgs_RemoveImagesAndVolumes(t *testing.T) {
	got := buildDownArgs(runtime.ComposeDownSpec{
		Files:         []string{"compose.yml"},
		ProjectName:   "dc-x",
		RemoveImages:  true,
		RemoveVolumes: true,
	})
	want := []string{
		"compose", "--project-name", "dc-x", "-f", "compose.yml",
		"down", "--rmi", "local", "--volumes",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestBuildDownArgs_OnlyVolumes(t *testing.T) {
	got := buildDownArgs(runtime.ComposeDownSpec{
		Files:         []string{"compose.yml"},
		ProjectName:   "dc-x",
		RemoveVolumes: true,
	})
	for _, a := range got {
		if a == "--rmi" {
			t.Errorf("RemoveImages=false should not emit --rmi; got %v", got)
		}
	}
	if !contains(got, "--volumes") {
		t.Errorf("RemoveVolumes=true should emit --volumes; got %v", got)
	}
}

func TestBuildPsArgs(t *testing.T) {
	got := buildPsArgs(runtime.ComposePsSpec{
		Files:       []string{"compose.yml", "dc-run.yaml"},
		ProjectName: "dc-x",
	}, "app")
	want := []string{
		"compose", "--project-name", "dc-x",
		"-f", "compose.yml", "-f", "dc-run.yaml",
		"ps", "-q", "app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
