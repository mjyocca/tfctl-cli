// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package configurationarchive

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPack(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "main.tf"), []byte("terraform {}\n"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(source, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, ".git", "config"), []byte("secret"), 0o600))

	archive, err := Pack(source)
	require.NoError(t, err)
	path := archive.path
	t.Cleanup(func() { _ = archive.Close() })

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.NotEqual(t, source, filepath.Dir(path))
	require.Positive(t, archive.Metadata.CompressedSize)
	require.Equal(t, int64(len("terraform {}\n")), archive.Metadata.UncompressedSize)

	names := archiveNames(t, archive)
	require.Contains(t, names, "main.tf")
	require.NotContains(t, names, ".git/config")
	require.Equal(t, 1, archive.Metadata.Files)

	require.NoError(t, archive.Close())
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, archive.Close(), "Close must be idempotent")
}

func TestPackAppliesTerraformIgnore(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, ".terraformignore"), []byte("ignored.tf\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "ignored.tf"), []byte("secret"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "included.tf"), []byte("included"), 0o600))

	archive, err := Pack(source)
	require.NoError(t, err)
	t.Cleanup(func() { _ = archive.Close() })

	names := archiveNames(t, archive)
	require.Contains(t, names, ".terraformignore")
	require.Contains(t, names, "included.tf")
	require.NotContains(t, names, "ignored.tf")
}

func TestPackRejectsInvalidSource(t *testing.T) {
	t.Parallel()

	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		_, err := Pack(filepath.Join(t.TempDir(), "missing"))
		require.ErrorContains(t, err, "configuration directory")
	})

	t.Run("regular file", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "main.tf")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		_, err := Pack(path)
		require.ErrorContains(t, err, "must be a directory")
	})

	t.Run("symlink root", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires additional privileges")
		}
		t.Parallel()
		realSource := t.TempDir()
		link := filepath.Join(t.TempDir(), "source")
		require.NoError(t, os.Symlink(realSource, link))
		_, err := Pack(link)
		require.ErrorContains(t, err, "must not be a symbolic link")
	})
}

func TestPackSymlinkContainment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges")
	}
	t.Parallel()

	t.Run("internal symlink", func(t *testing.T) {
		t.Parallel()
		source := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(source, "target.tf"), []byte("safe"), 0o600))
		require.NoError(t, os.Symlink("target.tf", filepath.Join(source, "link.tf")))

		archive, err := Pack(source)
		require.NoError(t, err)
		t.Cleanup(func() { _ = archive.Close() })
		require.Contains(t, archiveNames(t, archive), "link.tf")
	})

	t.Run("external symlink", func(t *testing.T) {
		t.Parallel()
		source := t.TempDir()
		sentinel := filepath.Join(t.TempDir(), "outside-secret")
		require.NoError(t, os.WriteFile(sentinel, []byte("must-not-upload"), 0o600))
		require.NoError(t, os.Symlink(sentinel, filepath.Join(source, "outside")))

		_, err := Pack(source)
		require.ErrorContains(t, err, "symlink")
	})
}

func archiveNames(t *testing.T, archive *Archive) []string {
	t.Helper()
	r, err := archive.Open()
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()

	gz, err := gzip.NewReader(r)
	require.NoError(t, err)
	defer func() { require.NoError(t, gz.Close()) }()

	tr := tar.NewReader(gz)
	var names []string
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, header.Name)
	}
	return names
}
