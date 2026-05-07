// Package compose handles the Docker Compose source kind for
// devcontainer.json: parsing the user's compose project via
// compose-spec/compose-go and synthesizing override files that the
// engine layers on top during `docker compose up`.
//
// Compose runtime orchestration (`up`, `down`, `ps`) is delegated to
// runtime.ComposeRuntime — this package is parser + override generator
// only. See design/compose.md for the full design and the §13
// "future Go-native compose runtime" analysis.
package compose
