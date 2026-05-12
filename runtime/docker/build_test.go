package docker

import (
	"reflect"
	"testing"
)

func TestExtractBaseImages(t *testing.T) {
	cases := []struct {
		name      string
		dockerfile string
		buildArgs map[string]string
		want      []string
	}{
		{
			name: "literal FROM",
			dockerfile: `FROM alpine:3.20
RUN echo hi`,
			want: []string{"alpine:3.20"},
		},
		{
			name: "ARG default substituted into FROM",
			dockerfile: `ARG BASE=alpine:3.20
FROM $BASE
RUN echo hi`,
			want: []string{"alpine:3.20"},
		},
		{
			name: "buildArgs override ARG default",
			dockerfile: `ARG BASE=alpine:3.20
FROM ${BASE}`,
			buildArgs: map[string]string{"BASE": "debian:bookworm-slim"},
			want:      []string{"debian:bookworm-slim"},
		},
		{
			name: "FROM AS stage; subsequent FROM refers to stage",
			dockerfile: `FROM alpine:3.20 AS builder
RUN echo a > /a
FROM builder
RUN cat /a`,
			want: []string{"alpine:3.20"},
		},
		{
			name: "multi-stage with distinct base images",
			dockerfile: `FROM golang:1.25 AS build
FROM alpine:3.20`,
			want: []string{"golang:1.25", "alpine:3.20"},
		},
		{
			name: "FROM with --platform flag",
			dockerfile: `FROM --platform=linux/amd64 alpine:3.20`,
			want:       []string{"alpine:3.20"},
		},
		{
			name: "unresolved ARG dropped",
			dockerfile: `FROM $UNDEFINED_BASE
FROM alpine:3.20`,
			want: []string{"alpine:3.20"},
		},
		{
			name: "comments and blank lines ignored",
			dockerfile: `# comment
   # indented comment

FROM alpine:3.20`,
			want: []string{"alpine:3.20"},
		},
		{
			name: "lowercase from is recognized",
			dockerfile: `from alpine:3.20`,
			want:       []string{"alpine:3.20"},
		},
		{
			name:       "empty input",
			dockerfile: "",
			want:       nil,
		},
		{
			name: "dedup repeated FROMs",
			dockerfile: `FROM alpine:3.20 AS a
FROM alpine:3.20 AS b`,
			want: []string{"alpine:3.20"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractBaseImages(tc.dockerfile, tc.buildArgs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("extractBaseImages = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSubstituteArgs(t *testing.T) {
	args := map[string]string{"X": "alpine", "Y": "3.20"}
	cases := []struct {
		in, want string
	}{
		{"$X:$Y", "alpine:3.20"},
		{"${X}:${Y}", "alpine:3.20"},
		{"prefix-$X-suffix", "prefix-alpine-suffix"},
		{"$UNKNOWN", "$UNKNOWN"},
		{"${UNKNOWN}", "${UNKNOWN}"},
		{"$", "$"},
		{"no-vars-here", "no-vars-here"},
	}
	for _, tc := range cases {
		got := substituteArgs(tc.in, args)
		if got != tc.want {
			t.Errorf("substituteArgs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
