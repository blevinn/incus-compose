package client

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	incusApi "github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/osarch"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	rspecs "github.com/opencontainers/runtime-spec/specs-go"
)

// BuildMode controls how build-configured images are treated during Ensure.
type BuildMode int

const (
	// BuildAuto builds the image only when it is missing (default).
	BuildAuto BuildMode = iota
	// BuildForce rebuilds the image even if an existing one is present.
	BuildForce
	// BuildNever never builds; returns an error if the image is missing.
	BuildNever
)

// BuildInfo carries the rebuild mode and optional builder selection for ActionEnsure.
type BuildInfo struct {
	// Mode controls rebuild behavior.
	Mode BuildMode

	// PreferredBuilder is the container builder binary name or absolute path.
	// Empty means auto-detect (tries podman, then docker).
	PreferredBuilder string
}

// BuildConfig holds the parameters read from a compose service's build: block.
type BuildConfig struct {
	// Context is the build context directory (absolute path).
	Context string

	// Dockerfile is an optional path to the Containerfile/Dockerfile, passed to
	// the builder verbatim, so a relative path resolves against the process
	// working directory rather than Context. Empty means the builder uses its
	// default (Containerfile or Dockerfile in Context).
	Dockerfile string

	// DockerfileInline is inline Dockerfile content from compose build.dockerfile_inline.
	DockerfileInline string

	// Target is the Dockerfile stage to build.
	Target string

	// Platform is the OCI platform to build for, for example linux/amd64.
	Platform string

	// Args are build-time variables (--build-arg).
	Args map[string]string

	// NoCache disables layer caching during the build as well as caching
	// the resulting image on the server.
	NoCache bool

	// Pull always attempts to pull a newer version of the base image.
	Pull bool

	// CustomBuilder is the binary name or path for custom builder invocation.
	CustomBuilder string

	// CustomBuilderArgs are arguments passed verbatim to the custom builder.
	CustomBuilderArgs []string
}

// buildArchitectures are the architectures a container builder takes a
// --platform for. Incus runs more than these, and a build targeting one of the
// others has nowhere to go, so it fails rather than handing the builder a
// platform it will reject.
var buildArchitectures = []string{"x86_64", "i686", "aarch64", "armv6l", "armv7l", "ppc64le", "s390x", "riscv64"}

// platformParts splits an architecture token into its canonical registry
// spelling and the variant that spelling implies. Incus' names and aliases
// carry one where the registry keeps it separate: armhf is ARMv7.
func platformParts(arch string) (string, string, bool) {
	id, err := osarch.ArchitectureID(arch)
	if err != nil {
		return "", "", false
	}

	name, err := osarch.ArchitectureName(id)
	if err != nil {
		return "", "", false
	}

	switch name {
	case "x86_64":
		return "amd64", "", true
	case "i686":
		return "386", "", true
	case "aarch64":
		return "arm64", "", true
	case "armv6l":
		return "arm", "v6", true
	case "armv7l":
		return "arm", "v7", true
	case "armv8l":
		return "arm", "v8", true
	}

	// ppc64le, s390x, riscv64, loongarch64 and the mips family spell the same
	// on both sides.
	return name, "", true
}

// platformKey normalizes an OCI platform to the form the cache alias suffix and
// the manifest picker share, and answers the Incus architecture alongside it.
//
// The variant is kept whenever the registry gives it its own manifest, which is
// every variant except the one case where it names the architecture itself:
// arm64's v8. A bare arm takes the variant its Incus architecture implies.
func platformKey(platform string) (string, string, bool) {
	parts := strings.Split(strings.ToLower(platform), "/")
	if parts[0] == "linux" {
		parts = parts[1:]
	}

	if len(parts) == 0 || len(parts) > 2 {
		return "", "", false
	}

	variant := ""
	if len(parts) == 2 {
		variant = parts[1]
	}

	base, implied, ok := platformParts(parts[0])
	if !ok {
		return "", "", false
	}

	if variant == "" {
		variant = implied
	}

	if base == "arm64" && variant == "v8" {
		variant = ""
	}

	key := base
	if variant != "" {
		key += "/" + variant
	}

	arch, ok := platformArch(key)
	if !ok {
		return "", "", false
	}

	return key, arch, true
}

// platformArch is the Incus architecture name for a platform key. Only 32-bit
// arm has an entry per variant; everywhere else the variant names a baseline of
// one architecture, which Incus does not model. Nor does it model every arm
// variant - v5 lands on ARMv6, the nearest it has - so this is not the answer
// to whether two keys name one manifest.
func platformArch(key string) (string, bool) {
	arch, variant, _ := strings.Cut(key, "/")

	if arch == "arm" {
		switch variant {
		case "v7":
			arch = "armv7l"
		case "v8":
			arch = "armv8l"
		default:
			arch = "armv6l"
		}
	}

	id, err := osarch.ArchitectureID(arch)
	if err != nil {
		return "", false
	}

	name, err := osarch.ArchitectureName(id)
	if err != nil {
		return "", false
	}

	return name, true
}

// archPlatform is the platform key for an Incus architecture, used when nothing
// asked for one and the server's own is taken instead.
func archPlatform(arch string) string {
	base, implied, ok := platformParts(arch)
	if !ok {
		return arch
	}

	if implied != "" {
		return base + "/" + implied
	}

	return base
}

// buildArtifacts holds the outputs of any container builder (internal or external).
type buildArtifacts struct {
	// Rootfs is a reader to the rootfs tar stream. Closing it removes temporary files.
	Rootfs io.ReadCloser

	// ConfigJSON is the serialized OCI runtime spec (rspecs.Spec) for metadata.tar.
	ConfigJSON []byte

	// Config is the OCI image configuration for Incus image properties.
	Config *ociImageConfig
}

// builderFunc executes an image build and returns the buildArtifacts.
type builderFunc func(ctx context.Context, c *Client, cfg *BuildConfig, imageName, arch string, stdout, stderr io.Writer) (*buildArtifacts, error)

// detectBuilder selects and prepares the appropriate image builder (docker builder or tar builder).
func detectBuilder(cfg *BuildConfig, preferred string) (builderFunc, error) {
	if cfg.CustomBuilder == "nix2container" {
		return runContainerNix, nil
	}
	if cfg.CustomBuilder != "" {
		p, err := exec.LookPath(cfg.CustomBuilder)
		if err != nil {
			return nil, fmt.Errorf("builder %q not found: %w", cfg.CustomBuilder, err)
		}
		return tarBuilder(p), nil
	}

	if preferred != "" {
		p, err := exec.LookPath(preferred)
		if err != nil {
			return nil, fmt.Errorf("builder %q not found: %w", preferred, err)
		}
		return dockerBuilder(p), nil
	}

	for _, name := range []string{"buildah", "podman", "docker"} {
		path, err := exec.LookPath(name)
		if err == nil {
			return dockerBuilder(path), nil
		}
	}

	return nil, fmt.Errorf("no container builder found; install podman, docker or buildah, or specify a custom builder")
}

// parseImageConfig converts loosely-decoded builder configuration into an
// ociImageConfig and an OCI runtime spec config.json bytes.
func parseImageConfig(imgCfg map[string]any, rootfsPath string) (*ociImageConfig, []byte, error) {
	if nested, ok := imgCfg["Config"].(map[string]any); ok && len(nested) > 0 {
		imgCfg = nested
	} else if nested, ok := imgCfg["config"].(map[string]any); ok && len(nested) > 0 {
		imgCfg = nested
	}

	toStrings := func(v any) []string {
		raw, _ := v.([]any)
		out := make([]string, 0, len(raw))
		for _, e := range raw {
			s, ok := e.(string)
			if ok {
				out = append(out, s)
			}
		}
		return out
	}

	cwd, _ := imgCfg["WorkingDir"].(string)
	if cwd == "" {
		cwd = "/"
	}

	user, _ := imgCfg["User"].(string)

	var uid, gid uint64
	if user != "" {
		split := strings.SplitN(user, ":", 2)
		uid, _ = strconv.ParseUint(split[0], 10, 32)
		if len(split) > 1 {
			gid, _ = strconv.ParseUint(split[1], 10, 32)
		}
	}

	env := slices.Clone(ociDefaultEnv)
	for _, entry := range toStrings(imgCfg["Env"]) {
		env = putEnv(env, entry, true)
	}

	home, err := rootfsHome(rootfsPath, uid)
	if err != nil {
		return nil, nil, fmt.Errorf("reading /etc/passwd from the built rootfs: %w", err)
	}
	if home != "" {
		env = putEnv(env, "HOME="+home, false)
	}

	rawVolumes, _ := imgCfg["Volumes"].(map[string]any)
	volumes := make(map[string]struct{}, len(rawVolumes))

	for path := range rawVolumes {
		volumes[path] = struct{}{}
	}

	hcRaw := imgCfg["Healthcheck"]
	if hcRaw == nil {
		hcRaw = imgCfg["healthcheck"]
	}
	if hcRaw == nil {
		hcRaw = imgCfg["HealthCheck"]
	}

	config := &ociImageConfig{
		ImageConfig: ocispec.ImageConfig{
			User:       user,
			Env:        env,
			Entrypoint: toStrings(imgCfg["Entrypoint"]),
			Cmd:        toStrings(imgCfg["Cmd"]),
			WorkingDir: cwd,
			Volumes:    volumes,
		},
		Healthcheck: buildHealthcheck(hcRaw),
	}

	configJSON, err := json.Marshal(rspecs.Spec{
		Version: rspecs.Version,
		Process: &rspecs.Process{
			Args: slices.Concat(config.Entrypoint, config.Cmd),
			Env:  env,
			Cwd:  cwd,
			User: rspecs.User{UID: uint32(uid), GID: uint32(gid)},
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling config.json: %w", err)
	}

	return config, configJSON, nil
}

// buildHealthcheck reads a Dockerfile HEALTHCHECK out of the loosely decoded
// image config, where builders report durations as nanosecond numbers.
func buildHealthcheck(v any) *ociHealthcheck {
	raw, _ := v.(map[string]any)
	if len(raw) == 0 {
		return nil
	}

	hc := &ociHealthcheck{}

	test, _ := raw["Test"].([]any)
	if len(test) == 0 {
		test, _ = raw["test"].([]any)
	}
	for _, e := range test {
		s, ok := e.(string)
		if ok {
			hc.Test = append(hc.Test, s)
		}
	}

	nanos := func(keys ...string) time.Duration {
		for _, key := range keys {
			if n, ok := raw[key].(float64); ok {
				return time.Duration(n)
			}
			if n, ok := raw[key].(int64); ok {
				return time.Duration(n)
			}
			if n, ok := raw[key].(int); ok {
				return time.Duration(n)
			}
			s, ok := raw[key].(string)
			if ok {
				d, err := time.ParseDuration(s)
				if err == nil {
					return d
				}
			}
		}

		return 0
	}

	hc.Interval = nanos("Interval", "interval")
	hc.Timeout = nanos("Timeout", "timeout")
	hc.StartPeriod = nanos("StartPeriod", "startPeriod", "start_period")
	hc.StartInterval = nanos("StartInterval", "startInterval", "start_interval")

	retries := func(keys ...string) int {
		for _, key := range keys {
			if n, ok := raw[key].(float64); ok {
				return int(n)
			}
			if n, ok := raw[key].(int); ok {
				return n
			}
			if n, ok := raw[key].(int64); ok {
				return int(n)
			}
			s, ok := raw[key].(string)
			if ok {
				n, err := strconv.Atoi(s)
				if err == nil {
					return n
				}
			}
		}

		return 0
	}

	hc.Retries = retries("Retries", "retries")

	return hc
}

// defaultSystemPath is the standard Linux PATH.
const defaultSystemPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// ociDefaultEnv is what umoci seeds a runtime spec with, so a built image gets the environment.* keys a pulled one does.
var ociDefaultEnv = []string{
	"PATH=" + defaultSystemPath,
	"TERM=xterm",
}

// putEnv sets entry in env, replacing a value already there only when clobber.
func putEnv(env []string, entry string, clobber bool) []string {
	name, _, ok := strings.Cut(entry, "=")
	if !ok {
		return env
	}

	for i, e := range env {
		if strings.HasPrefix(e, name+"=") {
			if clobber {
				env[i] = entry
			}
			return env
		}
	}
	return append(env, entry)
}

// rootfsHome returns uid's home from the rootfs tar's /etc/passwd, or "" when there is no entry.
func rootfsHome(path string, uid uint64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}

		if strings.TrimPrefix(hdr.Name, "./") != "etc/passwd" {
			continue
		}

		scanner := bufio.NewScanner(tr)
		for scanner.Scan() {
			fields := strings.Split(scanner.Text(), ":")
			if len(fields) >= 6 && fields[2] == strconv.FormatUint(uid, 10) {
				return fields[5], nil
			}
		}
		return "", scanner.Err()
	}
}

type tempFile struct {
	*os.File
	path string
	dir  string
}

// Close closes the file and removes it from disk.
func (t *tempFile) Close() error {
	err := t.File.Close()
	_ = os.Remove(t.path)
	if t.dir != "" {
		_ = os.RemoveAll(t.dir)
	}
	return err
}

// buildMetadataTar returns an in-memory tar containing metadata.yaml (JSON
// content per Incus convention) and, when provided, an OCI config.json.
func buildMetadataTar(name, arch string, configJSON []byte) (io.Reader, error) {
	metaJSON, err := json.Marshal(incusApi.ImageMetadata{
		Architecture: arch,
		CreationDate: time.Now().Unix(),
		Properties: map[string]string{
			"description": name + " (built by incus-compose)",
			"type":        "oci",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling image metadata: %w", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err = tw.WriteHeader(&tar.Header{
		Name: "metadata.yaml",
		Mode: 0o644,
		Size: int64(len(metaJSON)),
	})
	if err != nil {
		return nil, fmt.Errorf("writing metadata tar header: %w", err)
	}
	_, err = tw.Write(metaJSON)
	if err != nil {
		return nil, fmt.Errorf("writing metadata.yaml: %w", err)
	}

	if len(configJSON) > 0 {
		err = tw.WriteHeader(&tar.Header{
			Name: "config.json",
			Mode: 0o644,
			Size: int64(len(configJSON)),
		})
		if err != nil {
			return nil, fmt.Errorf("writing config.json tar header: %w", err)
		}
		_, err = tw.Write(configJSON)
		if err != nil {
			return nil, fmt.Errorf("writing config.json: %w", err)
		}
	}

	err = tw.Close()
	if err != nil {
		return nil, fmt.Errorf("closing metadata tar: %w", err)
	}
	return &buf, nil
}
