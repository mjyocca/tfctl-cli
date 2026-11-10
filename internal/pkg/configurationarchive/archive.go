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
	file      *os.File
	path      string
	closeOnce sync.Once
	closeErr  error
	Metadata  Metadata
}

// Pack validates and packages directory into a private temporary archive.
func Pack(directory string) (archive *Archive, err error) {
	source, err := validateSource(directory)
	if err != nil {
		return nil, err
	}

	file, path, err := createTemporaryArchive(source)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			err = errors.Join(err, cleanupTemporaryArchive(file, path))
		}
	}()

	// TODO: Enable strict .terraformignore handling when go-slug exposes an
	// option that returns read and parse errors instead of falling back to
	// default exclusions.
	packer, err := slug.NewPacker(slug.ApplyTerraformIgnore())
	if err != nil {
		return nil, fmt.Errorf("configure Terraform archive packer: %w", err)
	}
	meta, err := packer.Pack(source, file)
	if err != nil {
		return nil, fmt.Errorf("package configuration directory: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("flush temporary configuration archive: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect temporary configuration archive: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind temporary configuration archive: %w", err)
	}

	archive = &Archive{
		file: file,
		path: path,
		Metadata: Metadata{
			Files:            len(meta.Files),
			UncompressedSize: meta.Size,
			CompressedSize:   info.Size(),
		},
	}
	owned = true
	return archive, nil
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

func createTemporaryArchive(source string) (*os.File, string, error) {
	directories := []string{os.TempDir()}
	if cacheDirectory, err := os.UserCacheDir(); err == nil {
		directories = append(directories, cacheDirectory)
	}
	return createTemporaryArchiveIn(source, directories)
}

func createTemporaryArchiveIn(source string, directories []string) (*os.File, string, error) {
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

		withinSource, err := physicallyWithin(source, realDirectory)
		if err != nil {
			createErrors = append(createErrors, fmt.Errorf("check temporary directory %q: %w", realDirectory, err))
			continue
		}
		if withinSource {
			createErrors = append(createErrors, fmt.Errorf("temporary directory %q is inside the configuration directory", realDirectory))
			continue
		}

		file, err := os.CreateTemp(realDirectory, "tfctl-configuration-*.tar.gz")
		if err != nil {
			createErrors = append(createErrors, fmt.Errorf("create temporary configuration archive in %q: %w", realDirectory, err))
			continue
		}
		path := file.Name()
		withinSource, err = physicallyWithin(source, path)
		if err != nil || withinSource {
			cleanupErr := cleanupTemporaryArchive(file, path)
			if err != nil {
				createErrors = append(createErrors, errors.Join(fmt.Errorf("check temporary configuration archive: %w", err), cleanupErr))
			} else {
				createErrors = append(createErrors, errors.Join(errors.New("temporary configuration archive resolved inside the configuration directory"), cleanupErr))
			}
			continue
		}
		return file, path, nil
	}

	return nil, "", fmt.Errorf("create temporary configuration archive outside the configuration directory: %w", errors.Join(createErrors...))
}

func pathWithin(root, path string) (bool, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false, err
	}
	return relative == "." || relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
}

func physicallyWithin(root, path string) (bool, error) {
	within, err := pathWithin(root, path)
	if err != nil || within {
		return within, err
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		return false, err
	}
	current := path
	if info, err := os.Stat(current); err == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	for {
		info, err := os.Stat(current)
		if err != nil {
			return false, err
		}
		if os.SameFile(rootInfo, info) {
			return true, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
	}
}

func cleanupTemporaryArchive(file *os.File, path string) error {
	var cleanupErrors []error
	if file != nil {
		if err := file.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("close temporary configuration archive: %w", err))
		}
	}
	if path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove temporary configuration archive: %w", err))
		}
	}
	return errors.Join(cleanupErrors...)
}

// Reader returns the completed archive as a replayable reader. The reader is
// owned by Archive and remains valid until Close is called.
func (a *Archive) Reader() io.ReadSeeker {
	return a.file
}

// Close removes the temporary archive. It is safe to call more than once.
func (a *Archive) Close() error {
	a.closeOnce.Do(func() {
		a.closeErr = cleanupTemporaryArchive(a.file, a.path)
	})
	return a.closeErr
}
