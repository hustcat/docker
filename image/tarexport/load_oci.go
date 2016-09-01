package tarexport

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/docker/distribution"
	"github.com/docker/distribution/digest"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/progress"
	imgspec "github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func (l *tarexporter) loadOCI(tmpDir string, outStream io.Writer, progressOutput progress.Output) error {

	// FIXME(runcom): validate and check version of "oci-layout" file

	manifests := make(map[string]imgspecv1.Manifest)
	refsPath := filepath.Join(tmpDir, "refs")
	if err := filepath.Walk(refsPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		descriptor := imgspec.Descriptor{}
		if err := json.NewDecoder(f).Decode(&descriptor); err != nil {
			return err
		}
		// TODO(runcom): validate mediatype and size
		// TODO(runcom): validate digest not empty otherwise d.Algo.String panics below
		d := digest.Digest(descriptor.Digest)
		manifestPath := filepath.Join(tmpDir, "blobs", d.Algorithm().String(), d.Hex())
		f, err = os.Open(manifestPath)
		if err != nil {
			return err
		}
		defer f.Close()
		man := imgspecv1.Manifest{}
		if err := json.NewDecoder(f).Decode(&man); err != nil {
			return err
		}
		manifests[info.Name()] = man
		return nil
	}); err != nil {
		return err
	}

	var imageRefCount int
	var imageIDsStr string
	for ref, m := range manifests {
		// TODO(runcom): ref is a tag to be used below when registering tags
		_ = ref
		configDigest := digest.Digest(m.Config.Digest)
		config, err := ioutil.ReadFile(filepath.Join(tmpDir, "blobs", configDigest.Algorithm().String(), configDigest.Hex()))
		if err != nil {
			return err
		}
		img, err := image.NewFromJSON(config)
		if err != nil {
			return err
		}
		var rootFS image.RootFS
		rootFS = *img.RootFS
		rootFS.DiffIDs = nil
		if expected, actual := len(m.Layers), len(img.RootFS.DiffIDs); expected != actual {
			return fmt.Errorf("invalid manifest, layers length mismatch: expected %q, got %q", expected, actual)
		}
		for i, diffID := range img.RootFS.DiffIDs {
			layerDigest := digest.Digest(m.Layers[i].Digest)
			layerPath := filepath.Join(tmpDir, "blobs", layerDigest.Algorithm().String(), layerDigest.Hex())
			r := rootFS
			r.Append(diffID)
			newLayer, err := l.ls.Get(r.ChainID())
			if err != nil {
				// FIXME(runcom); 4th args is for foreign src!
				newLayer, err = l.loadLayer(layerPath, rootFS, diffID.String(), distribution.Descriptor{}, progressOutput)
				if err != nil {
					return err
				}
			}
			defer layer.ReleaseAndLog(l.ls, newLayer)
			if expected, actual := diffID, newLayer.DiffID(); expected != actual {
				return fmt.Errorf("invalid diffID for layer %d: expected %q, got %q", i, expected, actual)
			}
			rootFS.Append(diffID)
		}
		imgID, err := l.is.Create(config)
		if err != nil {
			return err
		}
		imageIDsStr += fmt.Sprintf("Loaded image ID: %s\n", imgID)

		// TODO(runcom): load tag!!! and increment imgRefCount
		imageRefCount = 0

		l.loggerImgEvent.LogImageEvent(imgID.String(), imgID.String(), "load")
	}

	if imageRefCount == 0 {
		outStream.Write([]byte(imageIDsStr))
	}
	return nil
}
