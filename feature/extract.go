package feature

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// extractTarball extracts a gzipped tarball into dst. Path traversal is
// rejected (entries with ".." components or absolute paths). Symlinks
// pointing outside dst are rejected. Mode bits are preserved on regular
// files; directories are created as 0o755.
//
// The spec mandates gzipped tarballs (.tgz) for both OCI and HTTPS
// feature distribution; plain tar is not supported.
//
// The reader is consumed; callers should close their underlying source
// after this returns.
func extractTarball(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	return extractTar(gz, dst)
}

func extractTar(r io.Reader, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}

		target, err := safeTargetPath(dstAbs, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return err
			}

		case tar.TypeSymlink, tar.TypeLink:
			// Validate the link target stays inside dst.
			linkTarget := hdr.Linkname
			if !filepath.IsAbs(linkTarget) {
				linkTarget = filepath.Join(filepath.Dir(target), linkTarget)
			}
			linkAbs, err := filepath.Abs(linkTarget)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(linkAbs+string(filepath.Separator), dstAbs+string(filepath.Separator)) && linkAbs != dstAbs {
				return fmt.Errorf("tarball symlink %q escapes destination", hdr.Name)
			}
			_ = os.Remove(target)
			if hdr.Typeflag == tar.TypeSymlink {
				if err := os.Symlink(hdr.Linkname, target); err != nil {
					return fmt.Errorf("symlink %s: %w", target, err)
				}
			} else {
				if err := os.Link(linkTarget, target); err != nil {
					return fmt.Errorf("hardlink %s: %w", target, err)
				}
			}

		default:
			// Skip device files, fifos, etc.
		}
	}
}

// safeTargetPath joins dst + name and rejects results that escape dst.
// Path-traversal protection.
func safeTargetPath(dst, name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("tarball entry %q has unsafe path", name)
	}
	target := filepath.Join(dst, clean)
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs+string(filepath.Separator), dst+string(filepath.Separator)) && abs != dst {
		return "", fmt.Errorf("tarball entry %q escapes destination", name)
	}
	return target, nil
}
