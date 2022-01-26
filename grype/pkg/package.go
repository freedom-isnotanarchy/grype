package pkg

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/anchore/grype/internal"
	"github.com/anchore/grype/internal/log"
	"github.com/anchore/syft/syft/pkg"
	"github.com/anchore/syft/syft/source"
)

// the source-rpm field has something akin to "util-linux-ng-2.17.2-12.28.el6_9.2.src.rpm"
// in which case the pattern will extract out the following values for the named capture groups:
//		name = "util-linux-ng"
//		version = "2.17.2" (or, if there's an epoch, we'd expect a value like "4:2.17.2")
//		release = "12.28.el6_9.2"
//		arch = "src"
var rpmPackageNamePattern = regexp.MustCompile(`^(?P<name>.*)-(?P<version>.*)-(?P<release>.*)\.(?P<arch>[a-zA-Z][^.]+)(\.rpm)$`)

// ID represents a unique value for each package added to a package catalog.
type ID string

// Package represents an application or library that has been bundled into a distributable format.
type Package struct {
	ID           ID
	Name         string            // the package name
	Version      string            // the version of the package
	Locations    []source.Location // the locations that lead to the discovery of this package (note: this is not necessarily the locations that make up this package)
	Language     pkg.Language      // the language ecosystem this package belongs to (e.g. JavaScript, Python, etc)
	Licenses     []string
	Type         pkg.Type  // the package type (e.g. Npm, Yarn, Python, Rpm, Deb, etc)
	CPEs         []pkg.CPE // all possible Common Platform Enumerators
	PURL         string    // the Package URL (see https://github.com/package-url/purl-spec)
	Upstreams    []UpstreamPackage
	MetadataType MetadataType
	Metadata     interface{} // This is NOT 1-for-1 the syft metadata! Only the select data needed for vulnerability matching
}

func New(p pkg.Package) Package {
	metadataType, metadata, upstreams := dataFromPkg(p)

	return Package{
		ID:           ID(p.ID()),
		Name:         p.Name,
		Version:      p.Version,
		Locations:    p.Locations,
		Licenses:     p.Licenses,
		Language:     p.Language,
		Type:         p.Type,
		CPEs:         p.CPEs,
		PURL:         p.PURL,
		Upstreams:    upstreams,
		MetadataType: metadataType,
		Metadata:     metadata,
	}
}

func FromCatalog(catalog *pkg.Catalog) []Package {
	result := make([]Package, 0, catalog.PackageCount())
	for _, p := range catalog.Sorted() {
		result = append(result, New(p))
	}
	return result
}

// Stringer to represent a package.
func (p Package) String() string {
	return fmt.Sprintf("Pkg(type=%s, name=%s, version=%s)", p.Type, p.Name, p.Version)
}

func dataFromPkg(p pkg.Package) (MetadataType, interface{}, []UpstreamPackage) {
	var metadata interface{}
	var upstreams []UpstreamPackage
	var metadataType MetadataType

	switch p.MetadataType {
	case pkg.DpkgMetadataType:
		upstreams = dpkgDataFromPkg(p)
	case pkg.RpmdbMetadataType:
		m, u := rpmdbDataFromPkg(p)
		upstreams = u
		if m != nil {
			metadata = *m
			metadataType = RpmdbMetadataType
		}
	case pkg.JavaMetadataType:
		if m := javaDataFromPkg(p); m != nil {
			metadata = *m
			metadataType = JavaMetadataType
		}
	case pkg.ApkMetadataType:
		upstreams = apkDataFromPkg(p)
	case "":
		// let's try to extract matching-specific information from additional sources other than syft json shapes.

		// TODO: add java cases here
		switch p.Type {
		case pkg.ApkPkg:
			upstreams = apkDataFromPURL(p.PURL)
		case pkg.DebPkg:
			upstreams = dpkgDataFromPURL(p.PURL)
		case pkg.RpmPkg:
			m, u := rpmdbDataFromPURL(p.PURL)
			upstreams = u
			if m != nil {
				metadata = *m
				metadataType = RpmdbMetadataType
			}
		}
	}
	return metadataType, metadata, upstreams
}

func dpkgDataFromPURL(p string) (upstreams []UpstreamPackage) {
	qualifiers := getPURLQualifiers(p)
	upstream := qualifiers[purlUpstreamQualifier]
	if upstream == "" {
		return nil
	}

	var sourceVersion string
	src := upstream

	fields := strings.SplitN(upstream, "@", 2)
	if len(fields) > 1 {
		src = fields[0]
		sourceVersion = fields[1]
	}

	return []UpstreamPackage{
		{
			Name:    src,
			Version: sourceVersion,
		},
	}
}

func dpkgDataFromPkg(p pkg.Package) (upstreams []UpstreamPackage) {
	if value, ok := p.Metadata.(pkg.DpkgMetadata); ok {
		if value.Source != "" {
			upstreams = append(upstreams, UpstreamPackage{
				Name:    value.Source,
				Version: value.SourceVersion,
			})
		}
	} else {
		log.Warnf("unable to extract DPKG metadata for %s", p)
	}
	return upstreams
}

func rpmdbDataFromPkg(p pkg.Package) (metadata *RpmdbMetadata, upstreams []UpstreamPackage) {
	if value, ok := p.Metadata.(pkg.RpmdbMetadata); ok {
		if value.SourceRpm != "" {
			name, version := getNameAndELVersion(value.SourceRpm)
			if name == "" && version == "" {
				log.Warnf("unable to extract name and version from SourceRPM=%q ", value.SourceRpm)
			} else if name != p.Name {
				// don't include matches if the source package name matches the current package name
				upstreams = append(upstreams, UpstreamPackage{
					Name:    name,
					Version: version,
				})
			}
		}
		if value.Epoch != nil {
			metadata = &RpmdbMetadata{Epoch: value.Epoch}
		}
	} else {
		log.Warnf("unable to extract RPM metadata for %s", p)
	}
	return metadata, upstreams
}

func rpmdbDataFromPURL(p string) (meta *RpmdbMetadata, upstreams []UpstreamPackage) {
	qualifiers := getPURLQualifiers(p)
	upstream := qualifiers[purlUpstreamQualifier]
	epoch := qualifiers[purlEpochQualifier]

	if epoch != "" {
		value, err := strconv.Atoi(epoch)
		if err != nil {
			log.Warnf("unable to parse RPM epoch=%q: %+v")
		} else {
			meta = &RpmdbMetadata{Epoch: &value}
		}
	}

	if upstream != "" {
		name, version := getNameAndELVersion(upstream)
		upstreams = append(upstreams, UpstreamPackage{
			Name:    name,
			Version: version,
		})
	}

	return meta, upstreams
}

func getNameAndELVersion(sourceRpm string) (string, string) {
	groupMatches := internal.MatchCaptureGroups(rpmPackageNamePattern, sourceRpm)
	version := groupMatches["version"] + "-" + groupMatches["release"]
	return groupMatches["name"], version
}

func javaDataFromPkg(p pkg.Package) (metadata *JavaMetadata) {
	if value, ok := p.Metadata.(pkg.JavaMetadata); ok {
		var artifact, group, name string
		if value.PomProperties != nil {
			artifact = value.PomProperties.ArtifactID
			group = value.PomProperties.GroupID
		}
		if value.Manifest != nil {
			if n, ok := value.Manifest.Main["Name"]; ok {
				name = n
			}
		}

		metadata = &JavaMetadata{
			VirtualPath:   value.VirtualPath,
			PomArtifactID: artifact,
			PomGroupID:    group,
			ManifestName:  name,
		}
	} else {
		log.Warnf("unable to extract Java metadata for %s", p)
	}
	return metadata
}

func apkDataFromPURL(p string) (upstreams []UpstreamPackage) {
	qualifiers := getPURLQualifiers(p)
	upstream := qualifiers[purlUpstreamQualifier]
	if upstream != "" {
		upstreams = append(upstreams, UpstreamPackage{
			Name: upstream,
		})
	}
	return upstreams
}

func apkDataFromPkg(p pkg.Package) (upstreams []UpstreamPackage) {
	if value, ok := p.Metadata.(pkg.ApkMetadata); ok {
		if value.OriginPackage != "" {
			upstreams = append(upstreams, UpstreamPackage{
				Name: value.OriginPackage,
			})
		}
	} else {
		log.Warnf("unable to extract APK metadata for %s", p)
	}
	return upstreams
}

func ByID(id ID, pkgs []Package) *Package {
	for _, p := range pkgs {
		if p.ID == id {
			return &p
		}
	}
	return nil
}
