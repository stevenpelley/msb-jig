// Package guest is what goes into the sandbox VM: provision/ (run by
// the supervisor at start, and by the image's Dockerfile at build), the
// image's Dockerfile, and mcpcli, the agent's MCP client.
package guest

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
)

// Provision is provision.sh and the files it sources (lib.sh,
// components/NN-<name>.sh), by path under provision/. A fresh map each
// call: a track replaces or adds a component in it.
func Provision() map[string]string {
	out := map[string]string{}
	err := fs.WalkDir(provisionFS, "provision", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := provisionFS.ReadFile(p)
		out[strings.TrimPrefix(p, "provision/")] = string(b)
		return err
	})
	if err != nil {
		panic(err)
	}
	return out
}

//go:embed provision
var provisionFS embed.FS

// ComponentName is the component a provision file holds
// (components/NN-<name>.sh: name), or "" for another file.
func ComponentName(path string) string {
	f, ok := strings.CutPrefix(path, "components/")
	if !ok || strings.Contains(f, "/") || !strings.HasSuffix(f, ".sh") {
		return ""
	}
	_, name, ok := strings.Cut(strings.TrimSuffix(f, ".sh"), "-")
	if !ok {
		return ""
	}
	return name
}

//go:embed image.Dockerfile
var imageDockerfile string

// layersMarker is the line in image.Dockerfile that ImageDockerfile
// replaces with the components' layers.
const layersMarker = "# COMPONENT LAYERS\n"

// ImageDockerfile builds the base image for microsandbox sandboxes from
// files (Provision's, under provision/ beside it), running the named
// components: one layer each, in provision.sh's order, so Docker's cache
// keeps every layer before the first changed component.
func ImageDockerfile(files map[string]string, components []string) string {
	var b strings.Builder
	for _, p := range slices.Sorted(maps.Keys(files)) {
		name := ComponentName(p)
		if name == "" || !slices.Contains(components, name) {
			continue
		}
		fmt.Fprintf(&b, "COPY provision/%s /opt/mono-jig/provision/components/\n", p)
		fmt.Fprintf(&b, "RUN SANDBOX_USER=\"$SANDBOX_USER\" SANDBOX_UID=\"$SANDBOX_UID\" PROVISION_COMPONENTS=%s \\\n", name)
		b.WriteString("    bash /opt/mono-jig/provision/provision.sh \\\n && rm -rf /var/lib/apt/lists/*\n")
	}
	return strings.Replace(imageDockerfile, layersMarker, b.String(), 1)
}

// ProvisionHash identifies a set of provision files, as the base image
// records it.
func ProvisionHash(files map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(h, "%d:%s%d:%s", len(k), k, len(files[k]), files[k])
	}
	return hex.EncodeToString(h.Sum(nil)[:6])
}

// MCPCLI is mcpcli, the agent's MCP client for the broker's AWS MCP
// endpoint, by file name (copied from the sbx-mcp-client skill).
var MCPCLI = map[string]string{"mcpcli": mcpcliWrapper, "mcpcli.py": mcpcliPy, "cwl_guarded_query.py": cwlGuardedQuery}

var (
	//go:embed mcpcli/mcpcli
	mcpcliWrapper string
	//go:embed mcpcli/mcpcli.py
	mcpcliPy string
	//go:embed mcpcli/cwl_guarded_query.py
	cwlGuardedQuery string
)
