package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDecodeStringOrStringArray(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"string", `"docker-compose.yml"`, []string{"docker-compose.yml"}},
		{"array", `["a.yml","b.yml"]`, []string{"a.yml", "b.yml"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeStringOrStringArray(json.RawMessage(tc.in))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecodeLifecycleCommand(t *testing.T) {
	t.Run("string => shell", func(t *testing.T) {
		cmd, err := decodeLifecycleCommand(json.RawMessage(`"echo hi"`))
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Single == nil || cmd.Single.Shell != "echo hi" {
			t.Errorf("got %+v", cmd)
		}
	})
	t.Run("array => exec", func(t *testing.T) {
		cmd, err := decodeLifecycleCommand(json.RawMessage(`["bash","-c","echo hi"]`))
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Single == nil || !reflect.DeepEqual(cmd.Single.Exec, []string{"bash", "-c", "echo hi"}) {
			t.Errorf("got %+v", cmd)
		}
	})
	t.Run("object => parallel", func(t *testing.T) {
		cmd, err := decodeLifecycleCommand(json.RawMessage(`{"setup":"setup.sh","probe":["probe","-v"]}`))
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Parallel["setup"].Shell != "setup.sh" {
			t.Errorf("setup shell mismatch: %+v", cmd.Parallel["setup"])
		}
		if !reflect.DeepEqual(cmd.Parallel["probe"].Exec, []string{"probe", "-v"}) {
			t.Errorf("probe exec mismatch: %+v", cmd.Parallel["probe"])
		}
	})
	t.Run("empty => empty", func(t *testing.T) {
		cmd, err := decodeLifecycleCommand(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !cmd.IsEmpty() {
			t.Errorf("expected empty, got %+v", cmd)
		}
	})
	t.Run("invalid type => error", func(t *testing.T) {
		_, err := decodeLifecycleCommand(json.RawMessage(`42`))
		if err == nil {
			t.Error("expected error for number")
		}
	})
}

func TestParseMountString(t *testing.T) {
	cases := []struct {
		in   string
		want Mount
	}{
		{
			in:   "type=bind,source=/host,target=/c",
			want: Mount{Type: MountBind, Source: "/host", Target: "/c"},
		},
		{
			// devpod-style src/dst aliases
			in:   "type=volume,src=mydata,dst=/data,readonly=true",
			want: Mount{Type: MountVolume, Source: "mydata", Target: "/data", ReadOnly: true},
		},
		{
			in:   "type=tmpfs,target=/run,ro=1",
			want: Mount{Type: MountTmpfs, Target: "/run", ReadOnly: true},
		},
	}
	for _, tc := range cases {
		got := parseMountString(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseMountString(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestDecodeMounts_Mixed(t *testing.T) {
	src := json.RawMessage(`[
		"type=bind,source=/host,target=/c",
		{"type":"volume","source":"data","target":"/data","readonly":true},
		42
	]`)
	mounts, warns, err := decodeMounts(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mounts) != 2 {
		t.Errorf("len(mounts) = %d, want 2 (number entry should be skipped)", len(mounts))
	}
	if len(warns) != 1 || warns[0].Code != WarnUnknownField {
		t.Errorf("expected one WarnUnknownField, got %v", warns)
	}
	if mounts[0].Type != MountBind || mounts[0].Source != "/host" {
		t.Errorf("mounts[0] = %+v", mounts[0])
	}
	if mounts[1].Type != MountVolume || !mounts[1].ReadOnly {
		t.Errorf("mounts[1] = %+v", mounts[1])
	}
}

func TestDecodeForwardPorts(t *testing.T) {
	src := json.RawMessage(`[3000, "127.0.0.1:8080", "9090", "bogus"]`)
	ports, warns, err := decodeForwardPorts(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []PortSpec{
		{Container: 3000},
		{Host: 0, Container: 0}, // see below
		{Container: 9090},
	}
	// "127.0.0.1:8080" — host part isn't a number; we treat it as invalid and warn.
	// The simpler "9090" works.
	// So the slice should have: 3000, 9090 (two valid), with two warnings.
	want = []PortSpec{
		{Container: 3000},
		{Container: 9090},
	}
	if !reflect.DeepEqual(ports, want) {
		t.Errorf("ports = %+v, want %+v", ports, want)
	}
	if len(warns) != 2 {
		t.Errorf("warnings = %v (want 2)", warns)
	}
}

func TestDecodeForwardPorts_HostContainer(t *testing.T) {
	src := json.RawMessage(`["8080:3000"]`)
	ports, warns, err := decodeForwardPorts(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if !reflect.DeepEqual(ports, []PortSpec{{Host: 8080, Container: 3000}}) {
		t.Errorf("got %+v", ports)
	}
}

func TestDecodeWorkspaceMount(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		m, err := decodeWorkspaceMount(json.RawMessage(`"type=bind,source=/h,target=/w"`))
		if err != nil {
			t.Fatal(err)
		}
		if m == nil || m.Type != MountBind || m.Source != "/h" || m.Target != "/w" {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("object", func(t *testing.T) {
		m, err := decodeWorkspaceMount(json.RawMessage(`{"type":"bind","source":"/h","target":"/w"}`))
		if err != nil {
			t.Fatal(err)
		}
		if m == nil || m.Type != MountBind || m.Target != "/w" {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("empty", func(t *testing.T) {
		m, err := decodeWorkspaceMount(nil)
		if err != nil || m != nil {
			t.Errorf("got m=%+v err=%v", m, err)
		}
	})
}
