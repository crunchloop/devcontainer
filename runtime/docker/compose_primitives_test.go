package docker

import (
	"testing"

	"github.com/crunchloop/devcontainer/runtime"
)

func TestLabelsMatch(t *testing.T) {
	tests := []struct {
		name string
		have map[string]string
		want map[string]string
		ok   bool
	}{
		{name: "exact match", have: map[string]string{"a": "1"}, want: map[string]string{"a": "1"}, ok: true},
		{name: "superset accepted", have: map[string]string{"a": "1", "b": "2"}, want: map[string]string{"a": "1"}, ok: true},
		{name: "missing key", have: map[string]string{"a": "1"}, want: map[string]string{"b": "2"}, ok: false},
		{name: "value mismatch", have: map[string]string{"a": "1"}, want: map[string]string{"a": "2"}, ok: false},
		{name: "empty want", have: map[string]string{"a": "1"}, want: nil, ok: true},
		{name: "empty both", have: nil, want: nil, ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := labelsMatch(tc.have, tc.want); got != tc.ok {
				t.Errorf("labelsMatch = %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestMapContainerState(t *testing.T) {
	cases := map[string]runtime.State{
		"created":    runtime.StateCreated,
		"running":    runtime.StateRunning,
		"paused":     runtime.StatePaused,
		"restarting": runtime.StateRestarting,
		"removing":   runtime.StateRemoving,
		"exited":     runtime.StateExited,
		"dead":       runtime.StateDead,
		"weird":      runtime.State("weird"),
	}
	for in, want := range cases {
		if got := mapContainerState(in); got != want {
			t.Errorf("mapContainerState(%q) = %q, want %q", in, got, want)
		}
	}
}
