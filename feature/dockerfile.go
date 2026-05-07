package feature

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/crunchloop/devcontainer/config"
)

// BuildPlan describes what to layer on top of a base image to produce
// a feature-extended devcontainer image.
type BuildPlan struct {
	// BaseImage is the reference of the image to build from. It will
	// be supplied as the build-arg _DEV_CONTAINERS_BASE_IMAGE.
	BaseImage string

	// Features are the resolved features in install order. Each must
	// have Dir + Metadata populated (i.e. fetched) unless
	// AlreadyInstalled is true, in which case it is skipped entirely.
	Features []config.ResolvedFeature

	// RemoteUser and ContainerUser feed into the final metadata label
	// entry. RemoteUser also drives the _DEV_CONTAINERS_IMAGE_USER
	// arg so the final image's USER matches the spec's intent.
	RemoteUser    string
	ContainerUser string
}

// HasWork reports whether plan has any features that still need
// installation. Useful for the engine to decide whether to skip the
// generated build entirely.
func (p BuildPlan) HasWork() bool {
	for _, f := range p.Features {
		if !f.AlreadyInstalled {
			return true
		}
	}
	return false
}

// GenerateBuildContext writes a Dockerfile and build-context directory
// to dst that, when built, produces a feature-extended image atop
// plan.BaseImage. dst must be empty or non-existent.
//
// Layout written:
//
//	dst/
//	  Dockerfile
//	  build-context/
//	    0/
//	      run.sh
//	      feature.env
//	      install.sh        (copied from feature.Dir)
//	      ...               (everything else from feature.Dir)
//	    1/
//	      ...
//
// AlreadyInstalled features are not copied or invoked but still
// consume an index slot so log lines / Dockerfile comments stay aligned
// with plan.Features.
func GenerateBuildContext(plan BuildPlan, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	bc := filepath.Join(dst, "build-context")
	if err := os.MkdirAll(bc, 0o755); err != nil {
		return err
	}

	for i, f := range plan.Features {
		if f.AlreadyInstalled {
			continue
		}
		if f.Dir == "" || f.Metadata.ID == "" {
			return fmt.Errorf("feature %s: must be fetched before generating build context (Dir/Metadata empty)", f.Ref)
		}
		idxDir := filepath.Join(bc, strconv.Itoa(i))
		if err := copyDir(f.Dir, idxDir); err != nil {
			return fmt.Errorf("copy feature %s: %w", f.Ref, err)
		}
		if err := os.WriteFile(filepath.Join(idxDir, "run.sh"), []byte(generateRunScript(f.Ref)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(idxDir, "feature.env"), SerializeEnvFile(f.Options), 0o644); err != nil {
			return err
		}
	}

	df, err := generateDockerfile(plan)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dst, "Dockerfile"), []byte(df), 0o644)
}

// generateDockerfile returns the contents of the Dockerfile that layers
// the configured features on top of plan.BaseImage. Structure:
//
//	# syntax=docker/dockerfile:1.4
//	ARG _DEV_CONTAINERS_BASE_IMAGE=...
//	FROM $_DEV_CONTAINERS_BASE_IMAGE AS devcontainer_target
//	USER root
//	COPY ./build-context/ /tmp/dc-features/
//	RUN chmod -R 0755 /tmp/dc-features
//	RUN echo "_CONTAINER_USER_HOME=..." >> /tmp/dc-features/builtin.env && ...
//	# per-feature: optional ENV + RUN cd /tmp/dc-features/<i> && ./run.sh
//	LABEL devcontainer.metadata='[...]'
//	ARG _DEV_CONTAINERS_IMAGE_USER=root
//	USER $_DEV_CONTAINERS_IMAGE_USER
func generateDockerfile(plan BuildPlan) (string, error) {
	var b strings.Builder
	b.WriteString("# syntax=docker/dockerfile:1.4\n")
	fmt.Fprintf(&b, "ARG _DEV_CONTAINERS_BASE_IMAGE=%s\n", plan.BaseImage)
	b.WriteString("FROM $_DEV_CONTAINERS_BASE_IMAGE AS devcontainer_target\n\n")

	b.WriteString("USER root\n\n")
	b.WriteString("COPY ./build-context/ /tmp/dc-features/\n")
	b.WriteString("RUN chmod -R 0755 /tmp/dc-features\n\n")

	b.WriteString("RUN echo \"_CONTAINER_USER_HOME=$(getent passwd root | cut -d: -f6)\" >> /tmp/dc-features/builtin.env && \\\n")
	b.WriteString("    echo \"_REMOTE_USER_HOME=$(getent passwd $(whoami) | cut -d: -f6)\" >> /tmp/dc-features/builtin.env\n\n")

	for i, f := range plan.Features {
		if f.AlreadyInstalled {
			fmt.Fprintf(&b, "# Feature %d: %s (already installed in base image; skipped)\n\n", i, f.Ref)
			continue
		}
		fmt.Fprintf(&b, "# Feature %d: %s\n", i, f.Ref)
		if len(f.Metadata.ContainerEnv) > 0 {
			b.WriteString("ENV ")
			keys := sortedStrings(f.Metadata.ContainerEnv)
			for j, k := range keys {
				if j > 0 {
					b.WriteString(" ")
				}
				fmt.Fprintf(&b, "%s=%s", k, strconv.Quote(f.Metadata.ContainerEnv[k]))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "RUN cd /tmp/dc-features/%d && chmod +x ./run.sh && ./run.sh\n\n", i)
	}

	label, err := buildMetadataLabel(plan)
	if err != nil {
		return "", fmt.Errorf("build metadata label: %w", err)
	}
	// Wrap the JSON in strconv-quoted form so embedded "/'/$ are unambiguous.
	fmt.Fprintf(&b, "LABEL %s=%s\n\n", MetadataLabel, strconv.Quote(string(label)))

	user := "root"
	if plan.RemoteUser != "" {
		user = plan.RemoteUser
	}
	fmt.Fprintf(&b, "ARG _DEV_CONTAINERS_IMAGE_USER=%s\n", user)
	b.WriteString("USER $_DEV_CONTAINERS_IMAGE_USER\n")

	return b.String(), nil
}

func sortedStrings(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// copyDir recursively copies src to dst, preserving file mode bits.
// Symlinks are resolved (we copy the target). Caller must ensure dst's
// parent exists; copyDir creates dst itself.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Resolve and copy the linked file.
			linkDst, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(linkDst) {
				linkDst = filepath.Join(filepath.Dir(p), linkDst)
			}
			data, err := os.ReadFile(linkDst)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFileContents(p, target, info.Mode().Perm())
	})
}

func copyFileContents(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
