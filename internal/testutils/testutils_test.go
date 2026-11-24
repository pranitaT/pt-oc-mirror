package testutils

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestRegistry starts an in-memory registry and returns its host (host:port).
func newTestRegistry(t *testing.T) string {
	t.Helper()
	server := CreateRegistry()
	t.Cleanup(server.Close)
	return registryHost(t, server)
}

func registryHost(t *testing.T, server *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	return u.Host
}

// newFakeImage returns a reproducible image whose content depends on content.
func newFakeImage(t *testing.T, content string) v1.Image { //nolint:ireturn // interface type is required by go-containerregistry
	t.Helper()
	img, err := crane.Image(map[string][]byte{"/arch-file": []byte("test contents " + content)})
	require.NoError(t, err)
	return img
}

func digestOf(t *testing.T, h interface{ Digest() (v1.Hash, error) }) string {
	t.Helper()
	d, err := h.Digest()
	require.NoError(t, err)
	return d.String()
}

// pushIndex builds an index with one child per architecture (linux), optionally
// converts it to the given media type, pushes it to ref and returns the expected
// digests: the index digest followed by the children digests, in order.
func pushIndex(t *testing.T, ref string, indexMediaType types.MediaType, archs ...string) []string {
	t.Helper()

	var idx v1.ImageIndex = empty.Index
	childDigests := make([]string, 0, len(archs))
	for _, arch := range archs {
		img := newFakeImage(t, arch)
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}},
		})
		childDigests = append(childDigests, digestOf(t, img))
	}
	if indexMediaType != "" {
		idx = mutate.IndexMediaType(idx, indexMediaType)
	}

	tag, err := name.NewTag(ref)
	require.NoError(t, err)
	require.NoError(t, remote.WriteIndex(tag, idx))

	return append([]string{digestOf(t, idx)}, childDigests...)
}

func TestGetAllManifestDigests_SingleImage(t *testing.T) {
	tests := []struct {
		name      string
		mediaType types.MediaType
	}{
		{name: "docker schema 2 manifest", mediaType: types.DockerManifestSchema2},
		{name: "oci manifest", mediaType: types.OCIManifestSchema1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := newTestRegistry(t) + "/single:latest"
			img := mutate.MediaType(newFakeImage(t, "single"), tt.mediaType)
			require.NoError(t, crane.Push(img, ref))

			digests, err := GetAllManifestDigests(ref)

			require.NoError(t, err)
			assert.Equal(t, []string{digestOf(t, img)}, digests)
		})
	}
}

// Guards the fixture used by the integration tests: an OCI index with exactly one child.
func TestGetAllManifestDigests_ExistingFixture(t *testing.T) {
	ref := newTestRegistry(t) + "/fixture:component1"
	content := map[string][]byte{"/fixture-file": []byte("test contents component1")}
	img, err := crane.Image(content)
	require.NoError(t, err)

	indexDigest, err := buildAndPushFakeImage(content, ref, t.TempDir())
	require.NoError(t, err)

	digests, err := GetAllManifestDigests(ref)

	require.NoError(t, err)
	assert.Equal(t, []string{indexDigest, digestOf(t, img)}, digests)
}

func TestGetAllManifestDigests_Index(t *testing.T) {
	tests := []struct {
		name      string
		mediaType types.MediaType
		archs     []string
	}{
		{name: "oci index amd64", mediaType: types.OCIImageIndex, archs: []string{"amd64"}},
		{name: "oci index ppc64le", mediaType: types.OCIImageIndex, archs: []string{"ppc64le"}},
		{name: "oci index arm64", mediaType: types.OCIImageIndex, archs: []string{"arm64"}},
		{name: "oci index multi-arch", mediaType: types.OCIImageIndex, archs: []string{"amd64", "ppc64le", "arm64"}},
		{name: "docker manifest list multi-arch", mediaType: types.DockerManifestList, archs: []string{"amd64", "ppc64le", "arm64"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := newTestRegistry(t) + "/multiarch:latest"
			expected := pushIndex(t, ref, tt.mediaType, tt.archs...)
			require.Len(t, expected, len(tt.archs)+1)

			digests, err := GetAllManifestDigests(ref)

			require.NoError(t, err)
			assert.Equal(t, expected, digests)
		})
	}
}

// An index without children is valid: only the index digest is returned.
func TestGetAllManifestDigests_EmptyIndex(t *testing.T) {
	ref := newTestRegistry(t) + "/empty:latest"
	expected := pushIndex(t, ref, "")
	require.Len(t, expected, 1)

	digests, err := GetAllManifestDigests(ref)

	require.NoError(t, err)
	assert.Equal(t, expected, digests)
}

// stubManifestRegistry serves body with the given Content-Type for every manifest request.
func stubManifestRegistry(t *testing.T, contentType, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return registryHost(t, server)
}

func TestGetAllManifestDigests_MalformedIndex(t *testing.T) {
	for _, mediaType := range []types.MediaType{types.OCIImageIndex, types.DockerManifestList} {
		t.Run(string(mediaType), func(t *testing.T) {
			ref := stubManifestRegistry(t, string(mediaType), `{"schemaVersion": 2, "manifests": [not json`) + "/malformed:latest"

			digests, err := GetAllManifestDigests(ref)

			require.Error(t, err)
			assert.Nil(t, digests, "no partial digest list may be returned with an error")
			assert.ErrorContains(t, err, "parsing index manifest of")
			assert.ErrorContains(t, err, ref)
			assert.ErrorContains(t, err, string(mediaType))
		})
	}
}

func TestGetAllManifestDigests_UnexpectedContentType(t *testing.T) {
	validIndex := `{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": [` +
		`{"mediaType": "application/vnd.oci.image.manifest.v1+json", "size": 1, ` +
		`"digest": "sha256:e033aa62f84267cf44de611acac2e76bfa4d2f0b6b2b61f1c4fecbefefde7159"}]}`

	tests := []struct {
		name        string
		contentType string
	}{
		{name: "generic json", contentType: "application/json"},
		{name: "plain text", contentType: "text/plain"},
		{name: "docker schema 1", contentType: string(types.DockerManifestSchema1)},
		{name: "docker schema 1 signed", contentType: string(types.DockerManifestSchema1Signed)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The body is a valid index: it must still not be silently treated as a single image.
			ref := stubManifestRegistry(t, tt.contentType, validIndex) + "/unexpected:latest"

			digests, err := GetAllManifestDigests(ref)

			require.Error(t, err)
			assert.Nil(t, digests, "no partial digest list may be returned with an error")
			assert.ErrorContains(t, err, "unsupported manifest media type")
			assert.ErrorContains(t, err, ref)
		})
	}
}

func TestGetAllManifestDigests_FetchError(t *testing.T) {
	ref := newTestRegistry(t) + "/missing:latest"

	digests, err := GetAllManifestDigests(ref)

	require.Error(t, err)
	assert.Nil(t, digests)
	assert.ErrorContains(t, err, "fetching manifest for")
	assert.ErrorContains(t, err, ref)

	var transportErr *transport.Error
	require.True(t, errors.As(err, &transportErr), "the registry error must stay reachable with errors.As")
	assert.Equal(t, http.StatusNotFound, transportErr.StatusCode)
}

func TestGetAllManifestDigests_InvalidReference(t *testing.T) {
	digests, err := GetAllManifestDigests("::not a valid reference::")

	require.Error(t, err)
	assert.Nil(t, digests)
	assert.ErrorContains(t, err, "parsing image reference")
}

