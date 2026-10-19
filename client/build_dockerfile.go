package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

func dockerBuilder(builder string) builderFunc {
	isBuildah := strings.HasSuffix(builder, "buildah")
	isPodman := strings.HasSuffix(builder, "podman")

	return func(ctx context.Context, c *Client, cfg *BuildConfig, imageName, arch string, stdout, stderr io.Writer) (*buildArtifacts, error) {
		isEnginePodman := isPodman || isBuildah
		tmpTag := fmt.Sprintf("ic-compose-build-%x", time.Now().UnixNano())

		rootfsTmp, err := os.CreateTemp("", "incus-compose-rootfs-*.tar")
		if err != nil {
			return nil, fmt.Errorf("creating temp file: %w", err)
		}
		rootfsPath := rootfsTmp.Name()
		err = rootfsTmp.Close()
		if err != nil {
			_ = os.Remove(rootfsPath)
			return nil, err
		}

		buildCfg, cleanup, err := buildConfigWithInlineDockerfile(cfg)
		if err != nil {
			_ = os.Remove(rootfsPath)
			return nil, err
		}
		defer cleanup()

		args := buildArgs(isEnginePodman, buildCfg, tmpTag, rootfsPath)
		c.LogDebug("Executing", "command", builder, "args", args)

		cmd := exec.CommandContext(ctx, builder, args...) //nolint:gosec
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if isEnginePodman {
			cmd.Env = append(os.Environ(), "BUILDAH_FORMAT=docker")
		}
		err = cmd.Run()
		if err != nil {
			_ = os.Remove(rootfsPath)
			return nil, fmt.Errorf("building container image: %w", err)
		}

		defer func() {
			c.LogDebug("Executing", "command", builder, "args", []string{"rmi", tmpTag})
			rmi := exec.CommandContext(ctx, builder, "rmi", tmpTag) //nolint:gosec
			rmi.Stdout = stdout
			rmi.Stderr = stderr
			_ = rmi.Run()
		}()

		inspect := exec.CommandContext(ctx, builder, "inspect", tmpTag) //nolint:gosec
		inspect.Stderr = stderr
		c.LogDebug("Executing", "command", builder, "args", inspect.Args[1:])
		out, err := inspect.Output()
		if err != nil {
			_ = os.Remove(rootfsPath)
			return nil, fmt.Errorf("inspecting built image: %w", err)
		}

		var imgCfg map[string]any
		if isBuildah {
			var inspected map[string]any
			err = json.Unmarshal(out, &inspected)
			if err != nil {
				_ = os.Remove(rootfsPath)
				return nil, fmt.Errorf("parsing buildah inspect output: %w", err)
			}
			ociv1, _ := inspected["OCIv1"].(map[string]any)
			imgCfg, _ = ociv1["config"].(map[string]any)
			if imgCfg == nil {
				imgCfg = make(map[string]any)
			}
			if imgCfg["Healthcheck"] == nil {
				if dockerVal, ok := inspected["Docker"].(map[string]any); ok {
					if dCfg, ok := dockerVal["config"].(map[string]any); ok && dCfg["Healthcheck"] != nil {
						imgCfg["Healthcheck"] = dCfg["Healthcheck"]
					} else if dCfg, ok := dockerVal["Config"].(map[string]any); ok && dCfg["Healthcheck"] != nil {
						imgCfg["Healthcheck"] = dCfg["Healthcheck"]
					}
				}
			}
		} else {
			var inspected []map[string]any
			err = json.Unmarshal(out, &inspected)
			if err != nil || len(inspected) == 0 {
				_ = os.Remove(rootfsPath)
				return nil, fmt.Errorf("parsing %s inspect output: %w", builder, err)
			}
			imgCfg, _ = inspected[0]["Config"].(map[string]any)
			if imgCfg == nil {
				imgCfg, _ = inspected[0]["config"].(map[string]any)
			}
		}

		config, configJSON, err := parseImageConfig(imgCfg, rootfsPath)
		if err != nil {
			_ = os.Remove(rootfsPath)
			return nil, err
		}

		f, err := os.Open(rootfsPath)
		if err != nil {
			_ = os.Remove(rootfsPath)
			return nil, fmt.Errorf("opening rootfs: %w", err)
		}

		return &buildArtifacts{
			Rootfs:     &tempFile{File: f, path: rootfsPath},
			ConfigJSON: configJSON,
			Config:     config,
		}, nil
	}
}

func buildConfigWithInlineDockerfile(cfg *BuildConfig) (*BuildConfig, func(), error) {
	if cfg.DockerfileInline == "" {
		return cfg, func() {}, nil
	}
	if cfg.Dockerfile != "" {
		return nil, func() {}, fmt.Errorf("build.dockerfile and build.dockerfile_inline cannot both be set")
	}

	f, err := os.CreateTemp("", "incus-compose-Dockerfile-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("creating inline Dockerfile: %w", err)
	}
	path := f.Name()
	_, err = f.WriteString(cfg.DockerfileInline)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, func() {}, fmt.Errorf("writing inline Dockerfile: %w", err)
	}
	err = f.Close()
	if err != nil {
		_ = os.Remove(path)
		return nil, func() {}, fmt.Errorf("closing inline Dockerfile: %w", err)
	}

	buildCfg := *cfg
	buildCfg.Dockerfile = path
	return &buildCfg, func() { _ = os.Remove(path) }, nil
}

func buildArgs(isPodman bool, cfg *BuildConfig, tmpTag, dest string) []string {
	args := []string{}
	if !isPodman {
		args = append(args, "buildx")
	}
	args = append(args, "build", "--tag", tmpTag)
	if cfg.Dockerfile != "" {
		args = append(args, "--file", cfg.Dockerfile)
	}
	if cfg.Platform != "" {
		args = append(args, "--platform", cfg.Platform)
	}
	if cfg.Target != "" {
		args = append(args, "--target", cfg.Target)
	}
	for k, v := range cfg.Args {
		args = append(args, "--build-arg", k+"="+v)
	}
	if cfg.NoCache {
		args = append(args, "--no-cache")
	}
	if cfg.Pull {
		args = append(args, "--pull")
	}
	if isPodman {
		args = append(args, "--format", "docker")
	}
	args = append(args, "--output", "type=tar,dest="+dest)
	if !isPodman {
		// buildx's --output alone only exports the tar; it does not load
		// the image into the local docker store the way buildah/podman's
		// own `build` command does regardless of --output. --load is
		// needed so the image can be inspected afterward for config.json.
		args = append(args, "--load")
	}
	args = append(args, cfg.Context)
	return args
}
