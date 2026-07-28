package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nkoteb/nimbus/internal/secrets"
)

// maxScanBytes bounds what is read per file. Nothing nimbus writes to the state
// repo is close to this, and a credential is not hiding past the first two
// megabytes of a file that is.
const maxScanBytes = 2 << 20

// SecretError refuses a commit that would publish a credential.
//
// It names the file and the line but never the value: an error message quoting
// the leak gets pasted into issue trackers and chat, which publishes the
// credential the refusal just prevented.
type SecretError struct {
	Path     string
	Findings []secrets.Finding
}

func (e *SecretError) Error() string {
	parts := make([]string, 0, len(e.Findings))
	for _, f := range e.Findings {
		parts = append(parts, f.String())
	}
	return fmt.Sprintf("refusing to commit %s: it contains %s\n"+
		"remove it, or encrypt it into the repo with `nimbus secrets encrypt`",
		e.Path, strings.Join(parts, " and "))
}

// scanForSecrets checks the files about to be staged.
//
// Only changed paths are read, so the cost is proportional to what is being
// written rather than to the size of the repo — which matters because this runs
// on every sync, including the daemon's.
func (r *Repo) scanForSecrets(changed []string) error {
	// Sorted so a tree with two offending files always reports the same one,
	// rather than whichever the map happened to yield first.
	paths := append([]string(nil), changed...)
	sort.Strings(paths)

	for _, rel := range paths {
		if skipScan(rel) {
			continue
		}
		data, err := readCapped(filepath.Join(r.Path, rel))
		if err != nil || secrets.Encrypted(data) {
			// A deleted file has nothing to leak, and an unreadable one is a
			// problem the commit itself will report more clearly.
			continue
		}
		if findings := secrets.Scan(data); len(findings) > 0 {
			return &SecretError{Path: rel, Findings: findings}
		}
	}
	return nil
}

// skipScan excludes paths whose contents are ciphertext by construction.
// secrets/ is the encrypted store (DESIGN.md §11); flagging it would mean the
// one directory built to hold credentials safely is the one that blocks sync.
func skipScan(rel string) bool {
	rel = filepath.ToSlash(rel)
	return rel == "secrets" || strings.HasPrefix(rel, "secrets/")
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, maxScanBytes)
	n, err := f.Read(buf)
	if n == 0 && err != nil {
		return nil, err
	}
	return buf[:n], nil
}
