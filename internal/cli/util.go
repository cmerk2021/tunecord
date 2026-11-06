package cli

import "os"

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
