package testutils

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/distribution/distribution/v3/manifest"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// WriteTestImage will use go-containerregistry to push a test image to
// an httptest.Server and will write the image to an OCI layout if dir is not "".
func WriteTestImage(testServer *httptest.Server, dir string) (string, error) {
	return WriteTestImageWithURL(testServer.URL, dir)
}

// WriteTestImageWithURL is similar to WriteTestImage, but is useful when testing
// with external registries rather than a httptest.Server
func WriteTestImageWithURL(URL string, dir string) (string, error) {
	u, err := url.Parse(URL)
	if err != nil {
		return "", err
	}
	c := map[string][]byte{
		"/testfile": []byte("test contents contents"),
	}
	targetRef := fmt.Sprintf("%s/bar:foo", u.Host)
	tag, err := name.NewTag(targetRef)
	if err != nil {
		return "", err
	}
	i, _ := crane.Image(c)

	if dir != "" {
		// create a new layout.Path and append the image to it
		lp, err := layout.Write(dir, empty.Index)
		if err != nil {
			return "", err
		}
		// write the image into the layout path
		if err := lp.AppendImage(i); err != nil {
			return "", err
		}
		// reload the path as an index and "push" it to remote
		idx, err := lp.ImageIndex()
		if err != nil {
			return "", err
		}
		if err := remote.WriteIndex(tag, idx); err != nil {
			return "", err
		}
	} else {
		// push image to remote
		if err := crane.Push(i, tag.String()); err != nil {
			return "", err
		}
	}
	return targetRef, nil
}

// WriteMultiarchTestImageWithURL will use go-containerregistry to push a multi arch test image to
// an httptest.Server and will write the image to an OCI layout if dir is not "".
func WriteMultiArchTestImage(testServer *httptest.Server, dir string) (string, error) {
	return WriteMultiArchTestImageWithURL(testServer.URL, dir)
}

// WriteMultiArchTestImageWithURL is similar to WriteMultiArchTestImage, but is useful when testing
// with external registries rather than a httptest.Server
func WriteMultiArchTestImageWithURL(URL string, dir string) (string, error) {
	u, err := url.Parse(URL)
	if err != nil {
		return "", err
	}
	targetRef := fmt.Sprintf("%s/bar:foo", u.Host)
	tag, err := name.NewTag(targetRef)
	if err != nil {
		return "", err
	}
	makeLayer := func() v1.Layer {
		layer, _ := random.Layer(100, types.DockerLayer)
		if err != nil {
			// this is a hack, but it should not happen
			panic(err)
		}
		return layer
	}
	img1, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "amd64"})
	if err != nil {
		return "", err
	}
	img1, err = mutate.Append(img1, mutate.Addendum{
		Layer: makeLayer(),
		History: v1.History{
			Author:    "random.Image",
			Comment:   fmt.Sprintf("this is a random history %d of %d", 1, 1),
			CreatedBy: "random",
			Created:   v1.Time{Time: time.Now()},
		},
		// MediaType: types.DockerManifestSchema2,
	})
	img2, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "ppc64le"})
	if err != nil {
		return "", err
	}
	img2, err = mutate.Append(img2, mutate.Addendum{
		Layer: makeLayer(),
		History: v1.History{
			Author:    "random.Image",
			Comment:   fmt.Sprintf("this is a random history %d of %d", 1, 1),
			CreatedBy: "random",
			Created:   v1.Time{Time: time.Now()},
		},
		// MediaType: types.DockerManifestSchema2,
	})
	ii := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add: img1,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{
					OS:           "linux",
					Architecture: "amd64",
				},
			},
		},
		mutate.IndexAddendum{
			Add: img2,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{
					OS:           "linux",
					Architecture: "ppc64le",
				},
			},
		},
	)
	// update the MediaType for this index
	ii = mutate.IndexMediaType(ii, types.DockerManifestList)
	if dir != "" {
		// "wrap" the newly created index in a new index (i.e. create manifest list indirection)
		// NOTE: manifest list indirection is only useful for OCI layouts... this is
		// not supported when pushing to a remote registry
		oci_ii := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
			Add: ii,
			Descriptor: v1.Descriptor{
				MediaType: types.DockerManifestList,
			},
		})
		_, err := layout.Write(dir, oci_ii)
		if err != nil {
			return "", err
		}
	}
	// now "push" to remote
	if err := remote.WriteIndex(tag, ii); err != nil {
		return "", err
	}
	return targetRef, nil
}

// RegistryFromFiles mirror a local V2 directory at the destintation with the source directory.
func RegistryFromFiles(source string) http.HandlerFunc {
	dir := http.Dir(source)
	fileHandler := http.FileServer(dir)
	handler := func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" && req.URL.Path == "/v2/" {
			w.Header().Set("Docker-Distribution-API-Version", "2.0")
		}
		if req.Method == "GET" {
			switch path.Base(path.Dir(req.URL.Path)) {
			case "blobs":
				w.Header().Set("Content-Type", "application/octet-stream")
			case "manifests":
				if f, err := dir.Open(req.URL.Path); err == nil {
					defer f.Close()
					if data, err := io.ReadAll(f); err == nil {
						var versioned manifest.Versioned
						if err = json.Unmarshal(data, &versioned); err == nil {
							w.Header().Set("Content-Type", versioned.MediaType)
						}
					}
				}
			}
		}
		fileHandler.ServeHTTP(w, req)
	}
	return http.HandlerFunc(handler)
}

// LocalMirrorFromFiles copies a local V2 directory to the destintation with the source directory.
func LocalMirrorFromFiles(source string, destination string) error {
	err := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		relPath := strings.Replace(path, source, "", 1)
		if relPath == "" {
			return nil
		}
		switch m := info.Mode(); {
		case m&fs.ModeSymlink != 0: // Tag is the file name, so follow the symlink to the layer ID-named file.
			dst, err := os.Readlink(path)
			if err != nil {
				return err
			}
			id := filepath.Base(dst)
			if err := os.Symlink(id, filepath.Join(destination, relPath)); err != nil {
				return err
			}
		case m.IsDir():
			return os.Mkdir(filepath.Join(destination, relPath), 0750)
		default:
			newSource := filepath.Join(source, relPath)
			cleanSource := filepath.Clean(newSource)
			data, err := os.ReadFile(cleanSource)
			if err != nil {
				return err
			}
			newDest := filepath.Join(destination, relPath)
			cleanDest := filepath.Clean(newDest)
			return os.WriteFile(cleanDest, data, 0600)
		}
		return nil
	})
	return err
}

func CreateRegistry() *httptest.Server {
	// Set up a fake registry.
	s := httptest.NewServer(registry.New())

	return s
}

func buildAndPushFakeImage(content map[string][]byte, imgRef string, dir string) (string, error) {
	var digest v1.Hash
	tag, err := name.NewTag(imgRef)
	if err != nil {
		return "", err
	}
	i, _ := crane.Image(content)
	if err := crane.Push(i, tag.String()); err != nil {
		return "", err
	}
	if dir == "" {
		digest, err = i.Digest()
		if err != nil {
			return "", err
		}
		return digest.String(), nil
	}
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		return "", err
	}
	if err := lp.AppendImage(i); err != nil {
		return "", err
	}
	idx, err := lp.ImageIndex()
	if err != nil {
		return "", err
	}
	if err := remote.WriteIndex(tag, idx); err != nil {
		return "", err
	}
	digest, err = idx.Digest()
	if err != nil {
		return "", err
	}

	return digest.String(), nil
}

// GetAllManifestDigests retrieves all manifest digests from a pushed image.
//
// The returned slice always starts with the digest of the referenced manifest:
//   - single image (OCI manifest or Docker schema 2): only that digest.
//   - image index / manifest list: that digest followed by the digest of every
//     child manifest, in index order. An empty index is valid and yields only
//     the index digest, because it has no children to report.
//
// Any other media type (e.g. Docker schema 1, or an unexpected Content-Type
// sent by the registry) is an error, as it cannot be determined whether child
// manifests exist. On error the returned slice is always nil: callers never
// receive a partial list together with a nil error.
func GetAllManifestDigests(imgRef string) ([]string, error) {
	ref, err := name.ParseReference(imgRef)
	if err != nil {
		return nil, fmt.Errorf("parsing image reference %q: %w", imgRef, err)
	}

	desc, err := remote.Get(ref)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest for %q: %w", imgRef, err)
	}

	digests := []string{desc.Digest.String()}

	if desc.MediaType.IsImage() {
		return digests, nil
	}

	if !desc.MediaType.IsIndex() {
		return nil, fmt.Errorf("unsupported manifest media type %q for %q (digest %s): expected an image or an image index",
			desc.MediaType, imgRef, desc.Digest)
	}

	idx, err := desc.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("converting %q (media type %s) to an image index: %w", imgRef, desc.MediaType, err)
	}

	idxManifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("parsing index manifest of %q (digest %s, media type %s): %w",
			imgRef, desc.Digest, desc.MediaType, err)
	}

	for _, m := range idxManifest.Manifests {
		digests = append(digests, m.Digest.String())
	}

	return digests, nil
}

// GenerateFakeImage will use go-containerregistry to push a test image to
// an httptest.Server and will write the image to an OCI layout if dir is not "".
func GenerateFakeImage(content, imgRef string, tempFolder string) (string, error) {
	imgSpec, err := image.ParseRef(imgRef)
	if err != nil {
		return "", err
	}
	sanitizedTagOrDigest := imgSpec.Tag
	if imgSpec.Tag == "" {
		sanitizedTagOrDigest = imgSpec.Digest
	} else {
		sanitizedTagOrDigest = strings.ReplaceAll(sanitizedTagOrDigest, ".", "-")
	}

	indexFolder := filepath.Join(tempFolder, imgSpec.PathComponent, sanitizedTagOrDigest)
	err = os.MkdirAll(indexFolder, 0o755)
	if err != nil {
		return "", err
	}
	c := map[string][]byte{
		"/testfile": []byte("test contents " + content),
	}
	return buildAndPushFakeImage(c, imgRef, indexFolder)
}

func ByteArrayFromTemplate(templatePath string, tokens []string) ([]byte, error) {
	exBytes := []byte{}
	buf := bytes.NewBuffer(exBytes)
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return []byte{}, err
	}
	err = tmpl.Execute(buf, tokens)
	if err != nil {
		return []byte{}, err
	}
	return buf.Bytes(), nil
}

func ImgRefsFromTemplate(filePath, templatePath string, token releaseContents) error {
	exBytes := []byte{}
	buf := bytes.NewBuffer(exBytes)
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return err
	}
	err = tmpl.Execute(buf, token)
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, buf.Bytes(), 0o600)
}

func FileFromTemplate(filePath, templatePath string, tokens []string) error {
	bytes, err := ByteArrayFromTemplate(templatePath, tokens)
	if err != nil {
		return err
	}

	fmt.Printf("registries.conf contents: %v", string(bytes))
	return os.WriteFile(filePath, bytes, 0o600)
}

func ImageExists(imgRef string) (bool, error) {
	desc, err := crane.Head(imgRef)
	if err != nil {
		return false, err
	}
	if desc != nil {
		return true, nil
	}
	return false, nil
}

type releaseContents struct {
	Ref1 string
	Ref2 string
	Ref3 string
}

func GenerateReleaseAndComponents(toRegistry, tempFolder string, templatePath string) (string, []string, error) {
	contents := releaseContents{}
	relatedImages := []string{}
	// generating a fake release with 3 components
	component1 := toRegistry + "/openshift-release-dev/ocp-v4.0-art-dev:component1"
	digest1, err := GenerateFakeImage("component1", component1, tempFolder)
	if err != nil {
		return "", relatedImages, err
	}
	contents.Ref1 = toRegistry + "/openshift-release-dev/ocp-v4.0-art-dev@" + digest1
	relatedImages = append(relatedImages, contents.Ref1)

	// Push signature images for all manifests in component1
	digests1, err := GetAllManifestDigests(component1)
	if err != nil {
		return "", relatedImages, fmt.Errorf("failed to get manifests for component1: %w", err)
	}
	for _, d := range digests1 {
		if err := PushSignatureImage(d, toRegistry, "openshift-release-dev/ocp-v4.0-art-dev"); err != nil {
			return "", relatedImages, fmt.Errorf("failed to push signature for component1 manifest %s: %w", d, err)
		}
	}

	component2 := toRegistry + "/openshift-release-dev/ocp-v4.0-art-dev:component2"
	digest2, err := GenerateFakeImage("component2", component2, tempFolder)
	if err != nil {
		return "", relatedImages, err
	}
	contents.Ref2 = toRegistry + "/openshift-release-dev/ocp-v4.0-art-dev@" + digest2
	relatedImages = append(relatedImages, contents.Ref2)

	// Push signature images for all manifests in component2
	digests2, err := GetAllManifestDigests(component2)
	if err != nil {
		return "", relatedImages, fmt.Errorf("failed to get manifests for component2: %w", err)
	}
	for _, d := range digests2 {
		if err := PushSignatureImage(d, toRegistry, "openshift-release-dev/ocp-v4.0-art-dev"); err != nil {
			return "", relatedImages, fmt.Errorf("failed to push signature for component2 manifest %s: %w", d, err)
		}
	}

	component3 := toRegistry + "/openshift-release-dev/ocp-v4.0-art-dev:component3"
	digest3, err := GenerateFakeImage("component3", component3, tempFolder)
	if err != nil {
		return "", relatedImages, err
	}
	contents.Ref3 = toRegistry + "/openshift-release-dev/ocp-v4.0-art-dev@" + digest3
	relatedImages = append(relatedImages, contents.Ref3)

	// Push signature images for all manifests in component3
	digests3, err := GetAllManifestDigests(component3)
	if err != nil {
		return "", relatedImages, fmt.Errorf("failed to get manifests for component3: %w", err)
	}
	for _, d := range digests3 {
		if err := PushSignatureImage(d, toRegistry, "openshift-release-dev/ocp-v4.0-art-dev"); err != nil {
			return "", relatedImages, fmt.Errorf("failed to push signature for component3 manifest %s: %w", d, err)
		}
	}

	releaseImg := toRegistry + "/openshift-release-dev/ocp-release:4.15.0-x86_64"
	digest, err := GenerateFakeRelease(contents, releaseImg, tempFolder, templatePath)
	if err != nil {
		return "", relatedImages, err
	}
	relatedImages = append(relatedImages, toRegistry+"/openshift-release-dev/ocp-release@"+digest)

	// Push signature images for all manifests in the release image
	releaseDigests, err := GetAllManifestDigests(releaseImg)
	if err != nil {
		return "", relatedImages, fmt.Errorf("failed to get manifests for release: %w", err)
	}
	for _, d := range releaseDigests {
		if err := PushSignatureImage(d, toRegistry, "openshift-release-dev/ocp-release"); err != nil {
			return "", relatedImages, fmt.Errorf("failed to push signature for release manifest %s: %w", d, err)
		}
	}

	return digest, relatedImages, nil
}

func GenerateFakeRelease(imageRefs releaseContents, releaseImgRef, tempFolder string, templateFile string) (string, error) {
	err := ImgRefsFromTemplate(tempFolder+"/image-references", templateFile, imageRefs)
	if err != nil {
		return "", err
	}
	imgSpec, err := image.ParseRef(releaseImgRef)
	if err != nil {
		return "", err
	}
	sanitizedTagOrDigest := imgSpec.Tag
	if imgSpec.Tag == "" {
		sanitizedTagOrDigest = imgSpec.Digest
	} else {
		sanitizedTagOrDigest = strings.ReplaceAll(sanitizedTagOrDigest, ".", "-")
	}

	indexFolder := filepath.Join(tempFolder, imgSpec.PathComponent, sanitizedTagOrDigest)
	err = os.MkdirAll(indexFolder, 0o755)
	if err != nil {
		return "", err
	}

	imageRefsBytes, err := os.ReadFile(tempFolder + "/image-references")
	if err != nil {
		return "", err
	}
	releaseMetaBytes, err := os.ReadFile("../../e2e/templates/release_templates/release-metadata")
	if err != nil {
		return "", err
	}
	c := map[string][]byte{
		"/release-manifests/image-references": imageRefsBytes,
		"/release-manifests/release-metadata": releaseMetaBytes,
	}
	return buildAndPushFakeImage(c, releaseImgRef, indexFolder)
}

type CincinnatiMock struct {
	Templates map[string]string
	Tokens    []string
}

func (c CincinnatiMock) CincinnatiHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	mtype := r.Header.Get("Accept")
	if mtype != GraphMediaType {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}

	keys, ok := r.URL.Query()["channel"]
	if !ok {
		// t.Fail()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	ch := keys[len(keys)-1]

	responseTemplate, ok := c.Templates[ch]
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	responseBytes, err := ByteArrayFromTemplate(responseTemplate, c.Tokens)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, err = w.Write(responseBytes)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

// PushSignatureImage creates and pushes a sigstore-style signature image to the registry.
// The signature tag format is: sha256-<digest>.sig
func PushSignatureImage(imgDigest, registryHost, repository string) error {
	// Parse the digest to create the signature tag
	digestStr := strings.TrimPrefix(imgDigest, "sha256:")
	sigTag := fmt.Sprintf("sha256-%s.sig", digestStr)

	// Create a minimal signature image (just a config layer)
	sigContent := map[string][]byte{
		"/signature": []byte("fake signature content for " + imgDigest),
	}

	// Build the full image reference with signature tag
	sigImgRef := fmt.Sprintf("%s/%s:%s", registryHost, repository, sigTag)

	tag, err := name.NewTag(sigImgRef)
	if err != nil {
		return fmt.Errorf("failed to create signature tag: %w", err)
	}

	// Create the signature image
	img, err := crane.Image(sigContent)
	if err != nil {
		return fmt.Errorf("failed to create signature image: %w", err)
	}

	// Convert to OCI manifest format
	ociImg := mutate.MediaType(img, types.OCIManifestSchema1)
	ociImg = mutate.ConfigMediaType(ociImg, types.OCIConfigJSON)

	// Push the image
	if err := remote.Write(tag, ociImg); err != nil {
		return fmt.Errorf("failed to push signature image: %w", err)
	}

	return nil
}

