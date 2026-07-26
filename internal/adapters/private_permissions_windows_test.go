//go:build windows

package adapters

func privatePathError(path string) error {
	// Owner ruling #13: Windows uses the OS-default DACL; ACL assertions are skipped.
	_ = path
	return nil
}
