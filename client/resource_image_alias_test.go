package client

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageAliasBounds(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"docker.io/library/alpine:latest",
		strings.Repeat("a", 64),
		strings.Repeat("a", 65),
		"ghcr.io/berriai/litellm@sha256:" + strings.Repeat("a", 64),
	} {
		alias := ImageAlias(name)
		require.LessOrEqual(t, len(alias), 64)
		if len(name) <= 64 {
			require.Equal(t, name, alias)
		} else {
			require.Len(t, alias, 61)
			require.NotEqual(t, ImageAlias(name+"b"), alias)
		}
	}
}

func TestDigestImageAliasPreservesSourceAndPlatform(t *testing.T) {
	t.Parallel()
	c := NewOfflineClient(t.Context(), "test")
	digest := "sha256:" + strings.Repeat("a", 64)
	image, err := newImage(c, "ghcr.io/berriai/litellm:v1.103.0@"+digest, &ImageConfig{})
	require.NoError(t, err)
	require.Equal(t, "ghcr.io", image.Remote())
	require.Equal(t, "berriai/litellm@"+digest, image.image)
	require.Equal(t, ImageAlias("ghcr.io/berriai/litellm@"+digest), image.IncusName())
	canonical, err := newImage(c, "ghcr.io/berriai/litellm@"+digest, &ImageConfig{})
	require.NoError(t, err)
	require.Equal(t, image.IncusName(), canonical.IncusName())
	aliases := map[string]bool{}
	for _, platform := range []string{"amd64", "arm64", "arm64/v8"} {
		image.platform = platform
		alias := image.cacheAlias()
		require.LessOrEqual(t, len(alias), 64)
		require.False(t, aliases[alias])
		aliases[alias] = true
	}
}

func TestDigestImageAliasEnsure(t *testing.T) {
	t.Parallel()
	skipLocal(t)
	c := newRandomTestClient(t, "digest-alias-")
	tag, err := newImage(c, "docker.io/library/busybox:latest", &ImageConfig{})
	require.NoError(t, err)
	err = tag.Ensure(t.Context(), OptionCreate())
	require.NoError(t, err)
	conn, err := c.Connection()
	require.NoError(t, err)
	img, _, err := conn.GetImage(t.Context(), c.incusProject, tag.State().IncusAlias.Target, nil)
	require.NoError(t, err)
	source := img.Properties["id"]
	require.Contains(t, source, "@sha256:")
	digest, err := newImage(c, tag.Remote()+"/"+source, &ImageConfig{})
	require.NoError(t, err)
	err = digest.Ensure(t.Context(), OptionCreate())
	require.NoError(t, err)
	require.Equal(t, tag.State().IncusAlias.Target, digest.State().IncusAlias.Target)
}
