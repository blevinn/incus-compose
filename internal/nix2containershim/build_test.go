package nix2containershim

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveNixSystem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		arch string
		want string
	}{
		{"amd64", "x86_64-linux"},
		{"x86_64", "x86_64-linux"},
		{"arm64", "aarch64-linux"},
		{"aarch64", "aarch64-linux"},
		{"armv7l", "armv7l-linux"},
		{"riscv64", "riscv64-linux"},
		{"ppc64le", "powerpc64le-linux"},
		{"s390x", "s390x-linux"},
		{"unknown", ""},
	}

	for _, tt := range tests {
		got := resolveNixSystem(tt.arch)
		assert.Equal(t, tt.want, got, "arch: %s", tt.arch)
	}
}

func TestHealthConfig_DurationUnmarshal(t *testing.T) {
	t.Parallel()

	dataNum := []byte(`{"Test":["CMD","true"],"Interval":10000000000,"Timeout":2000000000,"Retries":3}`)
	var hc1 HealthConfig
	err := json.Unmarshal(dataNum, &hc1)
	require.NoError(t, err)
	assert.Equal(t, Duration(10*time.Second), hc1.Interval)
	assert.Equal(t, Duration(2*time.Second), hc1.Timeout)
	assert.Equal(t, 3, hc1.Retries)

	dataStr := []byte(`{"Test":["CMD","true"],"Interval":"15s","Timeout":"5s","StartPeriod":"30s","Retries":5}`)
	var hc2 HealthConfig
	err = json.Unmarshal(dataStr, &hc2)
	require.NoError(t, err)
	assert.Equal(t, Duration(15*time.Second), hc2.Interval)
	assert.Equal(t, Duration(5*time.Second), hc2.Timeout)
	assert.Equal(t, Duration(30*time.Second), hc2.StartPeriod)
	assert.Equal(t, 5, hc2.Retries)
}

func TestUnhackNixCaseHack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "regular filename unchanged",
			input:    "config.json",
			expected: "config.json",
		},
		{
			name:     "case hack single digit",
			input:    "TEST~nix~case~hack~1",
			expected: "TEST",
		},
		{
			name:     "case hack multi digit",
			input:    "Makefile~nix~case~hack~12",
			expected: "Makefile",
		},
		{
			name:     "case hack invalid index zero",
			input:    "TEST~nix~case~hack~0",
			expected: "TEST~nix~case~hack~0",
		},
		{
			name:     "case hack invalid index negative",
			input:    "TEST~nix~case~hack~-1",
			expected: "TEST~nix~case~hack~-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := unhackNixCaseHack(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestFilePathToTarPath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/bin/sh", filePathToTarPath("/bin/sh", nil))

	opts := &PathOptions{
		Rewrite: &RewritePath{
			Regex: "^/nix/store/[^/]+",
			Repl:  "/usr",
		},
	}
	assert.Equal(t, "/usr/bin/bash", filePathToTarPath("/nix/store/12345-bash/bin/bash", opts))
}

func TestWriteRootfs_PathsOnly(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	binDir := filepath.Join(sourceDir, "bin")
	err := os.MkdirAll(binDir, 0o755)
	require.NoError(t, err)

	helloFile := filepath.Join(binDir, "hello")
	err = os.WriteFile(helloFile, []byte("echo hello\n"), 0o755)
	require.NoError(t, err)

	symlinkPath := filepath.Join(binDir, "hi")
	err = os.Symlink("hello", symlinkPath)
	require.NoError(t, err)

	img := &Image{
		Version: 1,
		Layers: []Layer{
			{
				Paths: []StorePath{
					{
						Path: sourceDir,
						Options: &PathOptions{
							Rewrite: &RewritePath{
								Regex: "^" + regexp.QuoteMeta(sourceDir),
								Repl:  "",
							},
							Perms: []PermPath{
								{
									Regex: ".*",
									UID:   1000,
									GID:   1000,
									Mode:  "0755",
								},
							},
						},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	err = writeRootfs(img, &buf)
	require.NoError(t, err)

	tr := tar.NewReader(&buf)
	foundHello := false
	foundHi := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		if hdr.Name == "bin/hello" {
			foundHello = true
			assert.Equal(t, 1000, hdr.Uid)
			assert.Equal(t, 1000, hdr.Gid)
			assert.Equal(t, int64(0o755), hdr.Mode)
			content, err := io.ReadAll(tr)
			require.NoError(t, err)
			assert.Equal(t, "echo hello\n", string(content))
		}

		if hdr.Name == "bin/hi" {
			foundHi = true
			assert.Equal(t, byte(tar.TypeSymlink), hdr.Typeflag)
			assert.Equal(t, "hello", hdr.Linkname)
		}
	}

	assert.True(t, foundHello, "expected bin/hello in tar archive")
	assert.True(t, foundHi, "expected bin/hi symlink in tar archive")
}
