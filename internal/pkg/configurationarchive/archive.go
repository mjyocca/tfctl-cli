// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

// Package configurationarchive creates Terraform-compatible configuration archives.
package configurationarchive

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	slug "github.com/hashicorp/go-slug"
)

// Metadata describes the contents and size of an archive.
type Metadata struct {
	Files            int
	UncompressedSize int64
	CompressedSize   int64
}

// Archive is a private, replayable configuration archive. Call Close to remove it.
type Archive struct {
	path      string
	closeOnce sync.Once
	closeErr  error
	Metadata  Metadata
}

// Pack validates and packages directory into a private temporary archive.
func Pack(directory string) (*Archive, error) {
	source, err := validateSource(directory)
	if err != nil {
		return nil, err
	}

	file, err := os.CreateTemp("", "tfctl-configuration-*.tar.gz")
	if err != nil {
		return nil, fmt.Errorf("create temporary configuration archive: %w", err)
	}
	path := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	if err := file.Chmod(0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("secure temporary configuration archive: %w", err)
	}

	// TODO: Enable strict .terraformignore handling when go-slug exposes an
	// option that returns read and parse errors instead of falling back to
	// default exclusions.
	packer, err := slug.NewPacker(slug.ApplyTerraformIgnore())
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("configure Terraform archive packer: %w", err)
	}
	meta, err := packer.Pack(source, file)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("package configuration directory: %w", err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("flush temporary configuration archive: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("inspect temporary configuration archive: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close temporary configuration archive: %w", err)
	}

	return &Archive{
		path: path,
		Metadata: Metadata{
			Files:            len(meta.Files),
			UncompressedSize: meta.Size,
			CompressedSize:   info.Size(),
		},
	}, nil
}

func validateSource(directory string) (string, error) {
	if directory == "" {
		return "", fmt.Errorf("configuration directory must not be empty")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve configuration directory: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("inspect configuration directory %q: %w", directory, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("configuration directory must not be a symbolic link")
	}
	if !info.IsDir() {
		return "", fmt.Errorf("configuration directory %q must be a directory", directory)
	}
	return abs, nil
}

// Open returns a new reader at the start of the completed archive.
func (a *Archive) Open() (io.ReadSeekCloser, error) {
	file, err := os.Open(a.path)
	if err != nil {
		return nil, fmt.Errorf("open temporary configuration archive: %w", err)
	}
	return file, nil
}

// Close removes the temporary archive. It is safe to call more than once.
func (a *Archive) Close() error {
	a.closeOnce.Do(func() {
		if err := os.Remove(a.path); err != nil && !os.IsNotExist(err) {
			a.closeErr = fmt.Errorf("remove temporary configuration archive: %w", err)
		}
	})
	return a.closeErr
}
