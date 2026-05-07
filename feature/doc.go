// Package feature implements the devcontainer feature pipeline:
// reference resolution (OCI / HTTPS / Local), option processing,
// dependency-graph ordering, and dockerfile generation.
//
// PR6 implements the local resolver, option merge, and DAG ordering
// (no network). OCI / HTTPS land in PR7; dockerfile-gen in PR8;
// devcontainer.metadata image-label read in PR9.
//
// See design/features.md for the full design.
package feature
