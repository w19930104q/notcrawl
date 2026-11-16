package exportgen

import "path/filepath"

// Verification helpers for export acceptance tests. The file ends in
// _test.go so the symbols are compiled only for test builds and never enter
// production binaries.

func readManifestAt(outputRoot, kind string) (Manifest, error) {
	return readManifest(filepath.Join(outputRoot, StateDirName, kind, "manifest.json"))
}

func sha256FileAt(path string) (string, int64, error) {
	return fingerprintFile(path)
}
