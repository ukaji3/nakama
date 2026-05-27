package awsx

import "os"

// lookupEnvNonEmpty returns os.Getenv(key) if it is non-empty, else "".
func lookupEnvNonEmpty(key string) string {
	v := os.Getenv(key)
	if v == "" {
		return ""
	}
	return v
}
