//go:build !windows

package cli

// HandleDoubleClick never applies outside Windows: other systems have
// a per-user folder on PATH and no Explorer to start nakama from.
func HandleDoubleClick() (code int, handled bool) {
	return 0, false
}
