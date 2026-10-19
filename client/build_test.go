package client

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/osarch"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func TestBuildMetadataTar(t *testing.T) {
	t.Parallel()
	r, err := buildMetadataTar("local/myproject-myservice:latest", "x86_64", nil)
	require.NoError(t, err)

	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	require.NoError(t, err)
	require.Equal(t, "metadata.yaml", hdr.Name)

	data, err := io.ReadAll(tr)
	require.NoError(t, err)

	var meta struct {
		Architecture string            `yaml:"architecture"`
		CreationDate int64             `yaml:"creation_date"`
		ExpiryDate   int64             `yaml:"expiry_date"`
		Properties   map[string]string `yaml:"properties"`
	}
	require.NoError(t, yaml.Unmarshal(data, &meta))

	require.Equal(t, "oci", meta.Properties["type"])
	require.Equal(t, "x86_64", meta.Architecture)
	require.Greater(t, meta.CreationDate, int64(0))
	require.Equal(t, int64(0), meta.ExpiryDate)
}

func TestBuildMetadataTarWithoutConfigJSON(t *testing.T) {
	t.Parallel()
	r, err := buildMetadataTar("test-image", "x86_64", nil)
	require.NoError(t, err)

	tr := tar.NewReader(r)
	_, err = tr.Next()
	require.NoError(t, err)

	_, err = tr.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestBuildMetadataTarWithConfigJSON(t *testing.T) {
	t.Parallel()
	configJSON := []byte(`{"ociVersion":"1.2.0","process":{"args":["/bin/sh"]}}`)
	r, err := buildMetadataTar("test-image", "x86_64", configJSON)
	require.NoError(t, err)

	tr := tar.NewReader(r)

	hdr, err := tr.Next()
	require.NoError(t, err)
	require.Equal(t, "metadata.yaml", hdr.Name)

	hdr, err = tr.Next()
	require.NoError(t, err)
	require.Equal(t, "config.json", hdr.Name)

	data, err := io.ReadAll(tr)
	require.NoError(t, err)
	require.Equal(t, configJSON, data)

	_, err = tr.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestOptionBuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode BuildMode
	}{
		{BuildAuto},
		{BuildForce},
		{BuildNever},
	}
	for _, tt := range tests {
		opts := NewOptions(OptionBuild(BuildInfo{Mode: tt.mode}))
		require.Equal(t, tt.mode, opts.Build.Mode)
	}
}

func TestBuildArchitectures(t *testing.T) {
	t.Parallel()

	// buildImage prefixes the key with linux/, so every entry has to spell a
	// platform a builder accepts.
	want := map[string]string{
		"x86_64":  "linux/amd64",
		"i686":    "linux/386",
		"aarch64": "linux/arm64",
		"armv7l":  "linux/arm/v7",
		"armv6l":  "linux/arm/v6",
		"ppc64le": "linux/ppc64le",
		"s390x":   "linux/s390x",
		"riscv64": "linux/riscv64",
	}

	for _, arch := range buildArchitectures {
		require.Equal(t, want[arch], "linux/"+archPlatform(arch), arch)
	}
	require.Len(t, want, len(buildArchitectures))

	// The architectures Incus runs and no builder targets must stay out, or the
	// build fails somewhere less clear than our own error.
	for _, arch := range []string{"loongarch64", "ppc", "ppc64", "mips", "mips64", "riscv32", "armv8l"} {
		require.NotContains(t, buildArchitectures, arch)
	}
}

func TestPlatformKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		platform string
		key      string
		arch     string
	}{
		{"linux/amd64", "amd64", "x86_64"},
		{"amd64", "amd64", "x86_64"},
		{"LINUX/AMD64", "amd64", "x86_64"},
		{"linux/386", "386", "i686"},
		{"linux/arm64", "arm64", "aarch64"},
		{"linux/arm/v7", "arm/v7", "armv7l"},
		{"linux/arm/v6", "arm/v6", "armv6l"},

		// Incus' architecture table is what says a bare arm is ARMv6.
		{"linux/arm", "arm/v6", "armv6l"},

		// v8 is arm64 itself, so the registry's two spellings are one key.
		{"linux/arm64/v8", "arm64", "aarch64"},

		// Everywhere else the variant stays: the registry serves a different
		// manifest for it, and the store keys on this string. busybox
		// publishes arm v5 beside v6 and v7, and Incus has no ARMv5.
		{"linux/amd64/v3", "amd64/v3", "x86_64"},
		{"linux/arm/v5", "arm/v5", "armv6l"},
		{"linux/arm/v9", "arm/v9", "armv6l"},

		// Incus' aliases for one architecture reach one key, so two spellings
		// cannot key two cache aliases for the same image.
		{"linux/x86_64", "amd64", "x86_64"},
		{"linux/armhf", "arm/v7", "armv7l"},
		{"linux/ppc64el", "ppc64le", "ppc64le"},
		{"linux/aarch64", "arm64", "aarch64"},

		// The architectures both sides spell the same way.
		{"linux/s390x", "s390x", "s390x"},
		{"linux/riscv64", "riscv64", "riscv64"},
		{"linux/loongarch64", "loongarch64", "loongarch64"},
	}
	for _, tt := range tests {
		key, arch, ok := platformKey(tt.platform)
		require.True(t, ok, tt.platform)
		require.Equal(t, tt.key, key, tt.platform)
		require.Equal(t, tt.arch, arch, tt.platform)
	}

	for _, bad := range []string{"", "linux", "windows/amd64", "linux/pdp11", "a/b/c/d"} {
		_, _, ok := platformKey(bad)
		require.False(t, ok, bad)
	}
}

func TestArchPlatform(t *testing.T) {
	t.Parallel()

	// The server's own architecture has to produce a key platformKey accepts,
	// or an image pulled without a platform cannot be found again.
	for _, arch := range osarch.SupportedArchitectures() {
		key := archPlatform(arch)

		got, back, ok := platformKey(key)
		require.True(t, ok, arch)
		require.Equal(t, key, got, arch)
		require.Equal(t, arch, back, arch)
	}

	require.Equal(t, "amd64", archPlatform("x86_64"))
	require.Equal(t, "arm64", archPlatform("aarch64"))
	require.Equal(t, "arm/v7", archPlatform("armv7l"))
	require.Equal(t, "s390x", archPlatform("s390x"))
}

func TestBuildArgs_Podman(t *testing.T) {
	t.Parallel()
	cfg := &BuildConfig{
		Context:    "/path/to/ctx",
		Dockerfile: "Containerfile",
		Args:       map[string]string{"FOO": "bar"},
		Platform:   "linux/amd64",
		Target:     "runtime",
		NoCache:    true,
		Pull:       false,
	}
	args := buildArgs(true, cfg, "ic-compose-build-test", "/tmp/out.tar")
	require.Contains(t, args, "build")
	require.Contains(t, args, "--tag")
	require.Contains(t, args, "ic-compose-build-test")
	require.Contains(t, args, "/path/to/ctx")
	require.Contains(t, args, "--file")
	require.Contains(t, args, "Containerfile")
	require.Contains(t, args, "--platform")
	require.Contains(t, args, "linux/amd64")
	require.Contains(t, args, "--target")
	require.Contains(t, args, "runtime")
	require.Contains(t, args, "--build-arg")
	require.Contains(t, args, "FOO=bar")
	require.Contains(t, args, "--no-cache")
	require.Contains(t, args, "--format")
	require.Contains(t, args, "docker")
	require.Contains(t, args, "type=tar,dest=/tmp/out.tar")
	require.NotContains(t, args, "buildx")
}

func TestBuildConfigWithInlineDockerfile(t *testing.T) {
	t.Parallel()
	cfg, cleanup, err := buildConfigWithInlineDockerfile(&BuildConfig{
		Context:          "/path/to/ctx",
		DockerfileInline: "FROM docker.io/alpine:latest\n",
	})
	require.NoError(t, err)
	defer cleanup()

	require.NotEmpty(t, cfg.Dockerfile)
	data, err := os.ReadFile(cfg.Dockerfile)
	require.NoError(t, err)
	require.Equal(t, "FROM docker.io/alpine:latest\n", string(data))
}

func TestBuildConfigWithInlineDockerfileRejectsDockerfile(t *testing.T) {
	t.Parallel()
	_, _, err := buildConfigWithInlineDockerfile(&BuildConfig{
		Dockerfile:       "Dockerfile",
		DockerfileInline: "FROM docker.io/alpine:latest\n",
	})
	require.Error(t, err)
}

func TestPutEnv(t *testing.T) {
	t.Parallel()

	env := putEnv(slices.Clone(ociDefaultEnv), "PATH=/opt/bin", true)
	require.Equal(t, []string{"PATH=/opt/bin", "TERM=xterm"}, env)

	env = putEnv(env, "TERM=dumb", false)
	require.Equal(t, []string{"PATH=/opt/bin", "TERM=xterm"}, env)

	env = putEnv(env, "HOME=/root", false)
	require.Equal(t, []string{"PATH=/opt/bin", "TERM=xterm", "HOME=/root"}, env)

	env = putEnv(env, "NOTANASSIGNMENT", true)
	require.Equal(t, []string{"PATH=/opt/bin", "TERM=xterm", "HOME=/root"}, env)
}

func writeRootfsTar(t *testing.T, files map[string]string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rootfs.tar")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	tw := tar.NewWriter(f)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}))
		_, err = tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())

	return path
}

func TestRootfsHome(t *testing.T) {
	t.Parallel()

	passwd := "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/sbin/nologin\napp:x:1000:1000::/home/app:/bin/sh\n"

	tests := []struct {
		name  string
		files map[string]string
		uid   uint64
		home  string
	}{
		{"root", map[string]string{"etc/passwd": passwd}, 0, "/root"},
		{"named user", map[string]string{"etc/passwd": passwd}, 1000, "/home/app"},
		{"dot prefixed entry", map[string]string{"./etc/passwd": passwd}, 0, "/root"},
		{"no entry for uid", map[string]string{"etc/passwd": passwd}, 42, ""},
		{"no passwd file", map[string]string{"etc/hosts": "127.0.0.1 localhost\n"}, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home, err := rootfsHome(writeRootfsTar(t, tt.files), tt.uid)
			require.NoError(t, err)
			require.Equal(t, tt.home, home)
		})
	}
}

func TestBuildArgs_Docker(t *testing.T) {
	t.Parallel()
	cfg := &BuildConfig{
		Context: "/path/to/ctx",
		Pull:    true,
	}
	args := buildArgs(false, cfg, "ic-compose-build-test", "/tmp/out.tar")
	require.Contains(t, args, "buildx")
	require.Contains(t, args, "build")
	require.Contains(t, args, "--pull")
	require.NotContains(t, args, "--no-cache")
}

func TestTempFile_Close(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp("", "incus-compose-test-*.tar")
	require.NoError(t, err)

	path := f.Name()
	tf := &tempFile{File: f, path: path}

	require.FileExists(t, path)

	err = tf.Close()
	require.NoError(t, err)
	require.NoFileExists(t, path)
}

func TestParseImageConfig_Healthcheck(t *testing.T) {
	t.Parallel()

	rootfsPath := writeRootfsTar(t, map[string]string{
		"etc/passwd": "root:x:0:0:root:/root:/bin/sh\napp:x:1000:1000::/home/app:/bin/sh\n",
	})

	cfgMap := map[string]any{
		"User":       "1000:1000",
		"WorkingDir": "/app",
		"Env":        []any{"FOO=bar"},
		"Entrypoint": []any{"/app/entrypoint"},
		"Cmd":        []any{"serve", "--port", "8080"},
		"Volumes":    map[string]any{"/data": map[string]any{}},
		"Healthcheck": map[string]any{
			"Test":        []any{"CMD-SHELL", "curl -f http://localhost:8080/health || exit 1"},
			"Interval":    float64(10 * time.Second),
			"Timeout":     float64(3 * time.Second),
			"StartPeriod": float64(5 * time.Second),
			"Retries":     float64(3),
		},
	}

	ociCfg, configJSON, err := parseImageConfig(cfgMap, rootfsPath)
	require.NoError(t, err)

	require.Equal(t, "1000:1000", ociCfg.User)
	require.Equal(t, "/app", ociCfg.WorkingDir)
	require.Equal(t, []string{"/app/entrypoint"}, ociCfg.Entrypoint)
	require.Equal(t, []string{"serve", "--port", "8080"}, ociCfg.Cmd)
	require.Contains(t, ociCfg.Volumes, "/data")

	require.NotNil(t, ociCfg.Healthcheck)
	require.Equal(t, []string{"CMD-SHELL", "curl -f http://localhost:8080/health || exit 1"}, ociCfg.Healthcheck.Test)
	require.Equal(t, 10*time.Second, ociCfg.Healthcheck.Interval)
	require.Equal(t, 3*time.Second, ociCfg.Healthcheck.Timeout)
	require.Equal(t, 5*time.Second, ociCfg.Healthcheck.StartPeriod)
	require.Equal(t, 3, ociCfg.Healthcheck.Retries)

	require.NotEmpty(t, configJSON)
	var spec map[string]any
	require.NoError(t, json.Unmarshal(configJSON, &spec))
	process, ok := spec["process"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []any{"/app/entrypoint", "serve", "--port", "8080"}, process["args"])
	require.Equal(t, "/app", process["cwd"])
}

func TestParseImageConfig_Healthcheck_LowerCase(t *testing.T) {
	t.Parallel()

	rootfsPath := writeRootfsTar(t, map[string]string{
		"etc/passwd": "root:x:0:0:root:/root:/bin/sh\n",
	})

	cfgMap := map[string]any{
		"healthcheck": map[string]any{
			"test":         []any{"CMD", "true"},
			"interval":     float64(5 * time.Second),
			"timeout":      float64(2 * time.Second),
			"start_period": float64(1 * time.Second),
			"retries":      float64(2),
		},
	}

	ociCfg, _, err := parseImageConfig(cfgMap, rootfsPath)
	require.NoError(t, err)

	require.NotNil(t, ociCfg.Healthcheck)
	require.Equal(t, []string{"CMD", "true"}, ociCfg.Healthcheck.Test)
	require.Equal(t, 5*time.Second, ociCfg.Healthcheck.Interval)
	require.Equal(t, 2*time.Second, ociCfg.Healthcheck.Timeout)
	require.Equal(t, 1*time.Second, ociCfg.Healthcheck.StartPeriod)
	require.Equal(t, 2, ociCfg.Healthcheck.Retries)
}

func TestParseImageConfig_Healthcheck_StringDurations(t *testing.T) {
	t.Parallel()

	rootfsPath := writeRootfsTar(t, map[string]string{
		"etc/passwd": "root:x:0:0:root:/root:/bin/sh\n",
	})

	cfgMap := map[string]any{
		"Healthcheck": map[string]any{
			"Test":          []any{"CMD-SHELL", "/bin/check"},
			"Interval":      "15s",
			"Timeout":       "4s",
			"StartPeriod":   "30s",
			"StartInterval": "1s",
			"Retries":       "5",
		},
	}

	ociCfg, _, err := parseImageConfig(cfgMap, rootfsPath)
	require.NoError(t, err)

	require.NotNil(t, ociCfg.Healthcheck)
	require.Equal(t, []string{"CMD-SHELL", "/bin/check"}, ociCfg.Healthcheck.Test)
	require.Equal(t, 15*time.Second, ociCfg.Healthcheck.Interval)
	require.Equal(t, 4*time.Second, ociCfg.Healthcheck.Timeout)
	require.Equal(t, 30*time.Second, ociCfg.Healthcheck.StartPeriod)
	require.Equal(t, 1*time.Second, ociCfg.Healthcheck.StartInterval)
	require.Equal(t, 5, ociCfg.Healthcheck.Retries)
}

func TestDetectBuilder_CustomBuilder(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	builderScript := filepath.Join(tmpDir, "mock-builder.sh")

	scriptContent := `#!/usr/bin/env bash
set -euo pipefail

out_dir=$(mktemp -d /tmp/mock-builder-XXXXXX)
config_file="${out_dir}/config.json"
rootfs_file="${out_dir}/rootfs.tar"

cat <<'EOF' > "${config_file}"
{
  "User": "0:0",
  "WorkingDir": "/",
  "Cmd": ["echo", "hello"],
  "Env": ["TARGETARCH=${TARGETARCH:-}", "CUSTOM_ARG=${CUSTOM_ARG:-}"],
  "Healthcheck": {
    "Test": ["CMD", "true"],
    "Interval": 5000000000
  }
}
EOF

tar -cf "${rootfs_file}" -C "${out_dir}" config.json

cat <<EOF
{
  "rootfs": "${rootfs_file}",
  "config": "${config_file}"
}
EOF
`
	err := os.WriteFile(builderScript, []byte(scriptContent), 0o755)
	require.NoError(t, err)

	client := &Client{incusProject: "test-proj", logger: slog.Default()}
	cfg := &BuildConfig{
		CustomBuilder:     builderScript,
		CustomBuilderArgs: []string{"arg1"},
		Args:              map[string]string{"CUSTOM_ARG": "custom_val"},
	}

	builder, err := detectBuilder(cfg, "")
	require.NoError(t, err)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	artifacts, err := builder(t.Context(), client, cfg, "test-img", "x86_64", &stdout, &stderr)
	require.NoError(t, err)
	require.NotNil(t, artifacts)
	require.NotNil(t, artifacts.Rootfs)
	require.NotNil(t, artifacts.Config)
	require.NotEmpty(t, artifacts.ConfigJSON)

	require.NotNil(t, artifacts.Config.Healthcheck)
	require.Equal(t, []string{"CMD", "true"}, artifacts.Config.Healthcheck.Test)
	require.Equal(t, 5*time.Second, artifacts.Config.Healthcheck.Interval)

	tf, ok := artifacts.Rootfs.(*tempFile)
	require.True(t, ok)
	rootfsPath := tf.path
	dirToClean := tf.dir

	require.FileExists(t, rootfsPath)
	err = artifacts.Rootfs.Close()
	require.NoError(t, err)

	require.NoFileExists(t, rootfsPath)
	if dirToClean != "" {
		require.NoDirExists(t, dirToClean)
	}
}

func TestDetectBuilder_CustomBuilderNotFound(t *testing.T) {
	t.Parallel()

	cfg := &BuildConfig{
		CustomBuilder: "nonexistent-builder-binary-12345",
	}

	_, err := detectBuilder(cfg, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestDetectBuilder_Preferred(t *testing.T) {
	t.Parallel()

	cfg := &BuildConfig{}
	builder, err := detectBuilder(cfg, "echo")
	require.NoError(t, err)
	require.NotNil(t, builder)
}

func TestDetectBuilder_PreferredNotFound(t *testing.T) {
	t.Parallel()

	cfg := &BuildConfig{}
	_, err := detectBuilder(cfg, "this-binary-does-not-exist-incus-compose-test")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestSelectContainerBuilder_Nix2Container(t *testing.T) {
	t.Parallel()

	cfg := &BuildConfig{CustomBuilder: "nix2container"}
	builder, err := detectBuilder(cfg, "")
	require.NoError(t, err)
	require.NotNil(t, builder)
}
