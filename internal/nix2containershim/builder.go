package nix2containershim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Artifacts carries the built rootfs archive and image configuration.
type Artifacts struct {
	// Rootfs is the opened rootfs tar archive. The caller must close it when done.
	Rootfs io.ReadCloser
	// RootfsPath is the local filesystem path to the rootfs tar archive.
	RootfsPath string
	// Config is the loosely-decoded image configuration (suitable for parseImageConfig).
	Config map[string]any
	// ConfigJSON is the raw indented image configuration JSON bytes.
	ConfigJSON []byte
}

type tempRootfs struct {
	*os.File
	path string
}

func (t *tempRootfs) Close() error {
	err := t.File.Close()
	if t.path != "" {
		_ = os.Remove(t.path)
	}
	return err
}

// Build compiles a Nix target for the specified architecture into rootfs and config artifacts.
func Build(ctx context.Context, dir, target, arch string, stdout, stderr io.Writer) (*Artifacts, error) {
	system := resolveNixSystem(arch)

	jsonPath, err := resolveOrBuildTarget(ctx, dir, target, system, stderr)
	if err != nil {
		return nil, fmt.Errorf("resolving target %s: %w", target, err)
	}

	img, err := readImageDescriptor(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("reading image descriptor %s: %w", jsonPath, err)
	}

	tmpFile, err := os.CreateTemp("", "nix2container-rootfs-*.tar")
	if err != nil {
		return nil, fmt.Errorf("creating temp rootfs: %w", err)
	}
	rootfsPath := tmpFile.Name()

	err = writeRootfs(img, tmpFile)
	if err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(rootfsPath)
		return nil, fmt.Errorf("writing rootfs: %w", err)
	}

	err = tmpFile.Close()
	if err != nil {
		_ = os.Remove(rootfsPath)
		return nil, fmt.Errorf("closing temp rootfs: %w", err)
	}

	f, err := os.Open(rootfsPath)
	if err != nil {
		_ = os.Remove(rootfsPath)
		return nil, fmt.Errorf("opening rootfs %s: %w", rootfsPath, err)
	}

	var configMap map[string]any
	var configJSON []byte

	if len(img.ImageConfig) > 0 {
		var buf bytes.Buffer
		err = json.Indent(&buf, img.ImageConfig, "", "  ")
		if err != nil {
			configJSON = img.ImageConfig
		} else {
			configJSON = buf.Bytes()
		}

		err = json.Unmarshal(img.ImageConfig, &configMap)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(rootfsPath)
			return nil, fmt.Errorf("unmarshaling image config: %w", err)
		}
	} else {
		configJSON = []byte("{}")
		configMap = make(map[string]any)
	}

	return &Artifacts{
		Rootfs:     &tempRootfs{File: f, path: rootfsPath},
		RootfsPath: rootfsPath,
		Config:     configMap,
		ConfigJSON: configJSON,
	}, nil
}

func readImageDescriptor(path string) (*Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading image descriptor %s: %w", path, err)
	}

	var img Image
	err = json.Unmarshal(data, &img)
	if err != nil {
		return nil, fmt.Errorf("parsing image descriptor %s: %w", path, err)
	}

	return &img, nil
}

func resolveNixSystem(arch string) string {
	switch strings.ToLower(arch) {
	case "amd64", "x86_64":
		return "x86_64-linux"
	case "arm64", "aarch64":
		return "aarch64-linux"
	case "arm", "armv6l", "arm/v6":
		return "armv6l-linux"
	case "armv7l", "armhf", "arm/v7":
		return "armv7l-linux"
	case "386", "i386", "i686":
		return "i686-linux"
	case "riscv64":
		return "riscv64-linux"
	case "ppc64le":
		return "powerpc64le-linux"
	case "s390x":
		return "s390x-linux"
	}
	return ""
}

func resolveOrBuildTarget(ctx context.Context, dir, target, system string, stderr io.Writer) (string, error) {
	cand := target
	if dir != "" && !filepath.IsAbs(target) {
		cand = filepath.Join(dir, target)
	}
	info, err := os.Stat(cand)
	if err == nil && !info.IsDir() && strings.HasSuffix(cand, ".json") {
		return cand, nil
	}

	args := []string{"build", "--no-link", "--print-out-paths"}
	if system != "" {
		args = append(args, "--system", system)
	}
	args = append(args, target)

	cmd := exec.CommandContext(ctx, "nix", args...) //nolint:gosec
	if dir != "" {
		cmd.Dir = dir
	}
	if stderr != nil {
		cmd.Stderr = stderr
	} else {
		cmd.Stderr = os.Stderr
	}

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix build failed: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		info, err = os.Stat(trimmed)
		if err != nil {
			continue
		}
		if !info.IsDir() && strings.HasSuffix(trimmed, ".json") {
			return trimmed, nil
		}
		if info.IsDir() {
			cand := filepath.Join(trimmed, "image.json")
			_, err = os.Stat(cand)
			if err == nil {
				return cand, nil
			}
			entries, err := os.ReadDir(trimmed)
			if err == nil {
				for _, e := range entries {
					if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
						return filepath.Join(trimmed, e.Name()), nil
					}
				}
			}
		}
	}

	return "", fmt.Errorf("no image JSON found in nix output paths: %s", string(out))
}
