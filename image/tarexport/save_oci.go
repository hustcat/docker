package tarexport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"regexp"

	oci "github.com/containers/image/oci/layout"
	ctypes "github.com/containers/image/types"
	"github.com/docker/distribution/digest"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/reference"
	imgspec "github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type layerInfo struct {
	digest string
	size   int64
}

type ociSaveSession struct {
	*tarexporter
	// string is a tag here
	//images       map[reference.Named]image.ID
	images       map[image.ID]*imageDescriptor
	name         string
	savedImages  map[image.ID][]byte // cache image.ID -> manifest bytes
	diffIDsCache map[layer.DiffID]*layerInfo
}

func (l *tarexporter) getRefs() (map[string]string, error) {
	refs := make(map[string]string)
	for image, ref := range l.refs {
		r, err := reference.ParseNamed(image)
		if err != nil {
			return nil, err
		}
		var tagged reference.NamedTagged
		if _, ok := r.(reference.Canonical); ok {
			continue // a digest reference it's unique, no need for a --ref
		}
		var ok bool
		if tagged, ok = r.(reference.NamedTagged); !ok {
			var err error
			if tagged, err = reference.WithTag(r, reference.DefaultTag); err != nil {
				return nil, err
			}
		}
		if !ociRefRegexp.MatchString(ref) {
			return nil, fmt.Errorf(`invalid reference "%s=%s", reference must not include characters outside of the set of "A" to "Z", "a" to "z", "0" to "9", the hyphen "-", the dot ".", and the underscore "_"`, image, ref)
		}
		refs[tagged.String()] = ref
	}
	return refs, nil
}

var ociRefRegexp = regexp.MustCompile(`^([A-Za-z0-9._-]+)+$`)

func (l *tarexporter) parseOCINames(names []string) (map[image.ID]*imageDescriptor, error) {
	refs, err := l.getRefs()
	if err != nil {
		return nil, err
	}
	imgDescr := make(map[image.ID]*imageDescriptor)
	tags := make(map[string]bool)

	addAssoc := func(id image.ID, ref reference.Named) error {
		if _, ok := imgDescr[id]; !ok {
			imgDescr[id] = &imageDescriptor{}
		}

		if ref != nil {
			var tagged reference.NamedTagged
			if _, ok := ref.(reference.Canonical); ok {
				return nil
			}
			var ok bool
			if tagged, ok = ref.(reference.NamedTagged); !ok {
				var err error
				if tagged, err = reference.WithTag(ref, reference.DefaultTag); err != nil {
					return nil
				}
			}

			r, ok := refs[tagged.String()]
			if ok {
				var err error
				if tagged, err = reference.WithTag(tagged, r); err != nil {
					return err
				}
			}

			for _, t := range imgDescr[id].refs {
				if tagged.String() == t.String() {
					return nil
				}
			}

			if tags[tagged.Tag()] {
				return fmt.Errorf("unable to include unique references %q in OCI image", tagged.Tag())
			}

			tags[tagged.Tag()] = true

			imgDescr[id].refs = append(imgDescr[id].refs, tagged)
		}

		return nil
	}

	// TODO(runcom): same as docker-save except the error return in addAssoc
	// and the tags map above.
	for _, name := range names {
		id, ref, err := reference.ParseIDOrReference(name)
		if err != nil {
			return nil, err
		}
		if id != "" {
			_, err := l.is.Get(image.ID(id))
			if err != nil {
				return nil, err
			}
			if err := addAssoc(image.ID(id), nil); err != nil {
				return nil, err
			}
			continue
		}
		if ref.Name() == string(digest.Canonical) {
			imgID, err := l.is.Search(name)
			if err != nil {
				return nil, err
			}
			if err := addAssoc(imgID, nil); err != nil {
				return nil, err
			}
			continue
		}
		if reference.IsNameOnly(ref) {
			assocs := l.rs.ReferencesByName(ref)
			for _, assoc := range assocs {
				if err := addAssoc(image.IDFromDigest(assoc.ID), assoc.Ref); err != nil {
					return nil, err
				}
			}
			if len(assocs) == 0 {
				imgID, err := l.is.Search(name)
				if err != nil {
					return nil, err
				}
				if err := addAssoc(imgID, nil); err != nil {
					return nil, err
				}
			}
			continue
		}
		id, err = l.rs.Get(ref)
		if err != nil {
			return nil, err
		}
		if err := addAssoc(image.IDFromDigest(id), ref); err != nil {
			return nil, err
		}
	}
	return imgDescr, nil
}

func (s *ociSaveSession) save(outStream io.Writer) error {
	s.diffIDsCache = make(map[layer.DiffID]*layerInfo)
	s.savedImages = make(map[image.ID][]byte)
	tempDir, err := ioutil.TempDir("", "oci-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	for id, info := range s.images {
		for _, i := range info.refs {
			ociRef, err := oci.NewReference(tempDir, i.Tag())
			if err != nil {
				return err
			}
			ociDest, err := ociRef.NewImageDestination(nil)
			if err != nil {
				return err
			}
			// TODO(runcom): handle foreign srcs like save.go
			if err := s.saveImage(id, ociDest); err != nil {
				return err
			}
		}
		if len(info.refs) == 0 {
			ociRef, err := oci.NewReference(tempDir, id.Digest().Hex())
			if err != nil {
				return err
			}
			ociDest, err := ociRef.NewImageDestination(nil)
			if err != nil {
				return err
			}
			// TODO(runcom): handle foreign srcs like save.go
			if err := s.saveImage(id, ociDest); err != nil {
				return err
			}
		}
	}

	fs, err := archive.Tar(tempDir, archive.Uncompressed)
	if err != nil {
		return err
	}
	defer fs.Close()

	_, err = io.Copy(outStream, fs)
	return err
}

func (s *ociSaveSession) saveImage(id image.ID, ociDest ctypes.ImageDestination) error {
	if m, ok := s.savedImages[id]; ok {
		// just add a new ref under refs/
		if err := ociDest.PutManifest(m); err != nil {
			return err
		}
		return nil
	}

	img, err := s.is.Get(id)
	if err != nil {
		return err
	}

	if len(img.RootFS.DiffIDs) == 0 {
		return fmt.Errorf("empty export - not implemented")
	}

	configJSON := img.RawJSON()
	configDigest, configSize, err := ociDest.PutBlob(bytes.NewReader(configJSON), "", -1)
	if err != nil {
		return err
	}

	// TODO(runcom): there should likely be a manifest builder (like docker/distribution)
	m := imgspecv1.Manifest{
		Versioned: imgspec.Versioned{
			SchemaVersion: 2,
			MediaType:     imgspecv1.MediaTypeImageManifest,
		},
		Config: imgspec.Descriptor{
			MediaType: imgspecv1.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      configSize,
		},
	}

	for i := range img.RootFS.DiffIDs {
		rootFS := *img.RootFS
		rootFS.DiffIDs = rootFS.DiffIDs[:i+1]

		l, err := s.ls.Get(rootFS.ChainID())
		if err != nil {
			return err
		}
		defer layer.ReleaseAndLog(s.ls, l)

		var (
			digest string
			size   int64
		)
		if i, ok := s.diffIDsCache[l.DiffID()]; ok {
			digest = i.digest
			size = i.size
		} else {
			arch, err := l.TarStream()
			if err != nil {
				return err
			}
			defer arch.Close()

			// FIXME: anywhere I can get a gzipped layer (and digest) as found in remote registries?

			pr, pw := io.Pipe()
			bufin := bufio.NewReader(arch)
			gw, err := archive.CompressStream(pw, archive.Gzip)
			if err != nil {
				return err
			}
			go func() {
				bufin.WriteTo(gw)
				gw.Close()
				pw.Close()
			}()

			digest, size, err = ociDest.PutBlob(pr, "", -1)
			if err != nil {
				return err
			}
			s.diffIDsCache[l.DiffID()] = &layerInfo{digest: digest, size: size}
		}

		descriptor := imgspec.Descriptor{
			MediaType: imgspecv1.MediaTypeImageLayer,
			Digest:    digest,
			Size:      size,
		}
		m.Layers = append(m.Layers, descriptor)
	}

	mJSON, err := json.Marshal(m)
	if err != nil {
		return err
	}

	if err := ociDest.PutManifest(mJSON); err != nil {
		return err
	}

	s.savedImages[id] = mJSON

	return nil
}
