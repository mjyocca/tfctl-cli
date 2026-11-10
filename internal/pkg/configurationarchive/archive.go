// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

// Package configurationarchive creates Terraform-compatible configuration archives.
package configurationarchive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	directory string
	closeOnce sync.Once
	closeErr  error
	Metadata  Metadata
}

type temporaryArchive struct {
	file      *os.File
	path      string
	directory string
}

// Pack validates and packages directory into a private temporary archive.
func Pack(directory string) (*Archive, error) {
	source, err := validateSource(directory)
	if err != nil {
		return nil, err
	}

	temporary, err := createTemporaryArchive(source)
	if err != nil {
		return nil, err
	}
	file := temporary.file
	path := temporary.path
	cleanup := temporary.cleanup
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
		cleanup()
		return nil, fmt.Errorf("close temporary configuration archive: %w", err)
	}

	return &Archive{
		path:      path,
		directory: temporary.directory,
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
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve configuration directory: %w", err)
	}
	return realPath, nil
}

func createTemporaryArchive(source string) (*temporaryArchive, error) {
	directories := []string{os.TempDir()}
	if cacheDirectory, err := os.UserCacheDir(); err == nil {
		directories = append(directories, cacheDirectory)
	}
	directories = append(directories, filepath.Dir(source))

	seen := make(map[string]struct{}, len(directories))
	var createErrors []error
	for _, directory := range directories {
		absoluteDirectory, err := filepath.Abs(directory)
		if err != nil {
			createErrors = append(createErrors, fmt.Errorf("resolve temporary directory %q: %w", directory, err))
			continue
		}
		realDirectory, err := filepath.EvalSymlinks(absoluteDirectory)
		if err != nil {
			createErrors = append(createErrors, fmt.Errorf("resolve temporary directory %q: %w", directory, err))
			continue
		}
		if _, ok := seen[realDirectory]; ok {
			continue
		}
		seen[realDirectory] = struct{}{}

		withinSource, err := pathWithin(source, realDirectory)
		if err != nil {
			createErrors = append(createErrors, fmt.Errorf("check temporary directory %q: %w", realDirectory, err))
			continue
		}
		if withinSource {
			createErrors = append(createErrors, fmt.Errorf("temporary directory %q is inside the configuration directory", realDirectory))
			continue
		}

		stagingDirectory, err := os.MkdirTemp(realDirectory, "tfctl-configuration-*")
		if err != nil {
			createErrors = append(createErrors, fmt.Errorf("create private temporary directory in %q: %w", realDirectory, err))
			continue
		}
		if err := os.Chmod(stagingDirectory, 0o700); err != nil {
			_ = os.Remove(stagingDirectory)
			createErrors = append(createErrors, fmt.Errorf("secure private temporary directory: %w", err))
			continue
		}

		realStagingDirectory, err := filepath.EvalSymlinks(stagingDirectory)
		if err != nil {
			_ = os.Remove(stagingDirectory)
			createErrors = append(createErrors, fmt.Errorf("resolve private temporary directory: %w", err))
			continue
		}
		withinSource, err = pathWithin(source, realStagingDirectory)
		if err != nil || withinSource {
			_ = os.Remove(stagingDirectory)
			if err != nil {
				createErrors = append(createErrors, fmt.Errorf("check private temporary directory: %w", err))
			} else {
				createErrors = append(createErrors, errors.New("private temporary directory resolved inside the configuration directory"))
			}
			continue
		}

		path := filepath.Join(realStagingDirectory, "configuration.tar.gz")
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = os.Remove(realStagingDirectory)
			createErrors = append(createErrors, fmt.Errorf("create temporary configuration archive: %w", err))
			continue
		}
		return &temporaryArchive{file: file, path: path, directory: realStagingDirectory}, nil
	}

	return nil, fmt.Errorf("create temporary configuration archive outside the configuration directory: %w", errors.Join(createErrors...))
}

func pathWithin(root, path string) (bool, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false, err
	}
	return relative == "." || relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
}

func (a *temporaryArchive) cleanup() {
	_ = a.file.Close()
	_ = os.Remove(a.path)
	_ = os.Remove(a.directory)
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
		var cleanupErrors []error
		if err := os.Remove(a.path); err != nil && !os.IsNotExist(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove temporary configuration archive: %w", err))
		}
		if err := os.Remove(a.directory); err != nil && !os.IsNotExist(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove private temporary directory: %w", err))
		}
		a.closeErr = errors.Join(cleanupErrors...)
	})
	return a.closeErr
}
