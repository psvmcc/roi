package roi

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/psvmcc/roi/internal/buildinfo"
)

func validFeedURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("invalid feed URL %q", raw)
	}
	return nil
}

// importURLExists compares validated subscription URLs, ignoring only the
// HTTP/HTTPS scheme. Other URL components retain their exact meaning.
func importURLExists(tx *sql.Tx, raw string) (bool, error) { return importURLExistsExcept(tx, raw, 0) }
func importURLExistsExcept(tx *sql.Tx, raw string, except int64) (bool, error) {
	scheme := "http"
	if strings.HasPrefix(raw, "http:") {
		scheme = "https"
	}
	alternate := scheme + raw[strings.IndexByte(raw, ':'):]
	var exists bool
	err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM feeds WHERE url IN (?,?) AND id<>?)", raw, alternate, except).Scan(&exists)
	return exists, err
}

var defaultUserAgent = buildinfo.UserAgent()

func validateUserAgent(value string) error {
	if len(value) > 512 {
		return clientError("user agent must not exceed 512 characters")
	}
	for _, c := range value {
		if c < 32 || c > 126 {
			return clientError("user agent must contain printable ASCII characters only")
		}
	}
	return nil
}

func validateIconOverride(value string) error {
	if len(value) > 8192 {
		return clientError("icon URL must not exceed 8192 bytes")
	}
	if value != "" {
		if err := validFeedURL(value); err != nil {
			return clientError(err.Error())
		}
	}
	return nil
}
