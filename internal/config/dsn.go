package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

const sessionSQLMode = "'STRICT_TRANS_TABLES,ONLY_FULL_GROUP_BY,NO_ENGINE_SUBSTITUTION'"

// errInvalidDSN is returned for every DSN the driver cannot parse. Driver
// error text echoes input (passwords, parameter values), so it is never used.
var errInvalidDSN = errors.New("invalid DSN (expected user:pass@tcp(host:port)/db?params)")

// allowedKeys are the DSN query keys a user may supply. multiStatements,
// allowAllFiles, allowCleartextPasswords, allowOldPasswords, interpolateParams,
// parseTime and the timeouts are always overwritten; charset must be utf8mb4,
// collation must match collationRe, tls and loc are kept as given.
var allowedKeys = map[string]bool{
	"multiStatements": true, "allowAllFiles": true, "allowCleartextPasswords": true,
	"allowOldPasswords": true, "interpolateParams": true, "parseTime": true,
	"timeout": true, "readTimeout": true, "writeTimeout": true,
	"tls": true, "loc": true, "collation": true, "charset": true,
}

// The driver concatenates the collation into "SET NAMES ... COLLATE <c>", so
// it must be a plain identifier.
var collationRe = regexp.MustCompile(`(?i)^utf8mb4_[a-z0-9_]+$`)

// normalizeDSN applies the DSN allowlist: unsafe driver flags are forced back
// to safe values (reported in warnings), read-only session parameters are
// injected, and any parameter not on the allowlist is an error. Errors and
// warnings never contain parameter values or credentials.
func normalizeDSN(raw string, queryTimeout time.Duration) (dsn string, warnings []string, err error) {
	if queryTimeout < time.Millisecond {
		// max_execution_time=0 would disable the server-side timeout.
		return "", nil, errors.New("query timeout must be at least 1ms")
	}
	// Check keys first: the driver panics on some (strict) before we could.
	if err := checkQueryKeys(raw); err != nil {
		return "", nil, err
	}
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		return "", nil, errInvalidDSN
	}
	if cfg.Collation != "" && !collationRe.MatchString(cfg.Collation) {
		return "", nil, errors.New(`unsupported DSN parameter "collation"`)
	}
	if cfg.TLSConfig == "skip-verify" || cfg.TLSConfig == "preferred" {
		warnings = append(warnings, `DSN parameter "tls" does not verify the server certificate`)
	}

	for _, f := range []struct {
		key string
		on  bool
	}{
		{"multiStatements", cfg.MultiStatements},
		{"allowAllFiles", cfg.AllowAllFiles},
		{"allowCleartextPasswords", cfg.AllowCleartextPasswords},
		{"allowOldPasswords", cfg.AllowOldPasswords},
		{"interpolateParams", cfg.InterpolateParams},
	} {
		if f.on {
			warnings = append(warnings, fmt.Sprintf("DSN parameter %q forced to false", f.key))
		}
	}

	cfg.MultiStatements = false
	cfg.AllowAllFiles = false
	cfg.AllowCleartextPasswords = false
	cfg.AllowOldPasswords = false
	cfg.InterpolateParams = false
	cfg.ParseTime = true
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = queryTimeout + 10*time.Second
	cfg.WriteTimeout = 10 * time.Second
	cfg.Params = map[string]string{
		"transaction_read_only": "1",
		"max_execution_time":    strconv.FormatInt(queryTimeout.Milliseconds(), 10),
		"sql_mode":              sessionSQLMode,
	}
	if err := cfg.Apply(mysql.Charset("utf8mb4", cfg.Collation)); err != nil {
		return "", nil, errInvalidDSN
	}
	return cfg.FormatDSN(), warnings, nil
}

// checkQueryKeys rejects every DSN query key that is not allowlisted, and a
// charset other than utf8mb4. mysql.Config does not expose charset or every
// driver option, so the raw query string is scanned the way the driver splits
// it (after the last '/', from the first '?'). A DSN without '/' is left to
// the parser to reject.
func checkQueryKeys(raw string) error {
	slash := strings.LastIndexByte(raw, '/')
	if slash < 0 {
		return nil
	}
	_, query, ok := strings.Cut(raw[slash+1:], "?")
	if !ok {
		return nil
	}
	// '@' or parentheses here mean the "query" is really part of a malformed
	// userinfo/address; naming a key from it could echo a password fragment.
	if strings.ContainsAny(query, "@()") {
		return errInvalidDSN
	}
	for kv := range strings.SplitSeq(query, "&") {
		key, value, found := strings.Cut(kv, "=")
		if !found {
			continue // the driver ignores these too
		}
		if !allowedKeys[key] {
			return fmt.Errorf("unsupported DSN parameter %q", key)
		}
		if key == "charset" && !strings.EqualFold(value, "utf8mb4") {
			return errors.New(`unsupported DSN parameter "charset"`)
		}
	}
	return nil
}
