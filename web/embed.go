// Package web embeds the template and static assets so the compiled binary
// is self-contained. The //go:embed directives must live in this package,
// adjacent to the files they embed.
package web

import (
	"embed"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"strconv"
)

//go:embed all:templates all:static
var embedded embed.FS

// Templates returns the templates subtree rooted at "templates".
func Templates() fs.FS {
	sub, err := fs.Sub(embedded, "templates")
	if err != nil {
		panic(err)
	}
	return sub
}

// Static returns the static assets subtree rooted at "static".
func Static() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// AssetVersion returns a short hash of the embedded static assets. It changes
// whenever any asset changes, so pages can reference /static URLs with a ?v=
// query that lets browsers cache assets for a year yet still pick up new
// versions immediately after a rebuild.
func AssetVersion() string {
	h := fnv.New64a()
	err := fs.WalkDir(embedded, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00", path)
		if !d.IsDir() {
			f, err := embedded.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(h, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	return strconv.FormatUint(h.Sum64(), 36)
}
