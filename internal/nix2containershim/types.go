// Package nix2containershim provides in-process rootfs and configuration compilation
// from nix2container image targets.
package nix2containershim

import (
	"encoding/json"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// HealthConfig matches Docker/OCI Schema 2 healthcheck definitions.
type HealthConfig struct {
	Test          []string `json:"Test,omitempty"`
	Interval      Duration `json:"Interval,omitempty"`
	Timeout       Duration `json:"Timeout,omitempty"`
	StartPeriod   Duration `json:"StartPeriod,omitempty"`
	StartInterval Duration `json:"StartInterval,omitempty"`
	Retries       int      `json:"Retries,omitempty"`
}

// Duration is a wrapper around time.Duration that supports both nanosecond numbers and duration strings.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var n int64
	err := json.Unmarshal(b, &n)
	if err == nil {
		*d = Duration(time.Duration(n))
		return nil
	}

	var s string
	err = json.Unmarshal(b, &s)
	if err == nil {
		dur, err := time.ParseDuration(s)
		if err == nil {
			*d = Duration(dur)
			return nil
		}
	}

	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).Nanoseconds())
}

// ImageConfig embeds standard OCI v1.ImageConfig and adds de-facto Healthcheck.
type ImageConfig struct {
	ocispec.ImageConfig
	Healthcheck *HealthConfig `json:"Healthcheck,omitempty"`
}

// Image represents the nix2container image descriptor (image.json).
type Image struct {
	Version     int             `json:"version"`
	ImageConfig json.RawMessage `json:"image-config"`
	Layers      []Layer         `json:"layers"`
	Arch        string          `json:"arch"`
	Created     *time.Time      `json:"created,omitempty"`
}

// Layer represents a single layer in nix2container.
type Layer struct {
	Digest    string      `json:"digest"`
	DiffIDs   string      `json:"diff_ids"`
	Size      int64       `json:"size"`
	Paths     []StorePath `json:"paths"`
	LayerPath string      `json:"layer-path"`
	MediaType string      `json:"mediaType"`
}

// StorePath represents a path added to a layer with optional rewrite and permissions.
type StorePath struct {
	Path    string       `json:"path"`
	Options *PathOptions `json:"options"`
}

// PathOptions specifies rewrite and permission options for a store path.
type PathOptions struct {
	Rewrite *RewritePath `json:"rewrite"`
	Perms   []PermPath   `json:"perms"`
}

// RewritePath specifies a regex replacement to apply to store paths.
type RewritePath struct {
	Regex string `json:"regex"`
	Repl  string `json:"repl"`
}

// PermPath specifies permission and ownership overrides matching a regex.
type PermPath struct {
	Regex  string `json:"regex"`
	Mode   string `json:"mode"`
	OrMode string `json:"orMode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Uname  string `json:"uname"`
	Gname  string `json:"gname"`
}
