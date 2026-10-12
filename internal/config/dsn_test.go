package config

import (
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

const wantSQLMode = "'STRICT_TRANS_TABLES,ONLY_FULL_GROUP_BY,NO_ENGINE_SUBSTITUTION'"

func mustNormalize(t *testing.T, raw string, qt time.Duration) (string, []string) {
	t.Helper()
	out, warns, err := normalizeDSN(raw, qt)
	if err != nil {
		t.Fatalf("normalizeDSN(%q): %v", raw, err)
	}
	return out, warns
}

func mustParse(t *testing.T, dsn string) *mysql.Config {
	t.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", dsn, err)
	}
	return cfg
}

// driverQuery returns the query string the way the driver finds it: after the
// last '/', from the first '?' (passwords may contain '/' and '?').
func driverQuery(dsn string) string {
	_, q, _ := strings.Cut(dsn[strings.LastIndexByte(dsn, '/')+1:], "?")
	return q
}

// queryOf returns the query-string values of a DSN (charset is not exposed by
// mysql.Config, so it is checked here).
func queryOf(t *testing.T, dsn string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(driverQuery(dsn))
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", dsn, err)
	}
	return v
}

func TestNormalizeDSNForcedSettings(t *testing.T) {
	out, warns := mustNormalize(t, "u:pw@tcp(h:3306)/db", 30*time.Second)
	cfg := mustParse(t, out)

	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
	for name, got := range map[string]bool{
		"MultiStatements":         cfg.MultiStatements,
		"AllowAllFiles":           cfg.AllowAllFiles,
		"AllowCleartextPasswords": cfg.AllowCleartextPasswords,
		"AllowOldPasswords":       cfg.AllowOldPasswords,
		"InterpolateParams":       cfg.InterpolateParams,
	} {
		if got {
			t.Errorf("%s = true, want false", name)
		}
	}
	if !cfg.ParseTime {
		t.Error("ParseTime = false, want true")
	}
	if cfg.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", cfg.Timeout)
	}
	if cfg.ReadTimeout != 40*time.Second {
		t.Errorf("ReadTimeout = %v, want 40s", cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != 10*time.Second {
		t.Errorf("WriteTimeout = %v, want 10s", cfg.WriteTimeout)
	}
	if cfg.User != "u" || cfg.Passwd != "pw" || cfg.Addr != "h:3306" || cfg.DBName != "db" {
		t.Errorf("connection target changed: %q %q %q %q", cfg.User, cfg.Passwd, cfg.Addr, cfg.DBName)
	}
	if got := queryOf(t, out)["charset"]; !reflect.DeepEqual(got, []string{"utf8mb4"}) {
		t.Errorf("charset = %v, want [utf8mb4]", got)
	}
	want := map[string]string{
		"transaction_read_only": "1",
		"max_execution_time":    "30000",
		"sql_mode":              wantSQLMode,
	}
	if !reflect.DeepEqual(cfg.Params, want) {
		t.Errorf("Params = %v, want %v", cfg.Params, want)
	}
}

func TestNormalizeDSNQueryTimeout(t *testing.T) {
	out, _ := mustNormalize(t, "u:pw@tcp(h:3306)/db", 2*time.Second)
	cfg := mustParse(t, out)
	if got := cfg.Params["max_execution_time"]; got != "2000" {
		t.Errorf("max_execution_time = %q, want 2000", got)
	}
	if cfg.ReadTimeout != 12*time.Second {
		t.Errorf("ReadTimeout = %v, want 12s", cfg.ReadTimeout)
	}
}

func TestNormalizeDSNRejectsSubMillisecondTimeout(t *testing.T) {
	// max_execution_time=0 would disable the server-side timeout.
	for _, qt := range []time.Duration{0, -time.Second, 500 * time.Microsecond} {
		if _, _, err := normalizeDSN("u:pw@tcp(h:3306)/db", qt); err == nil {
			t.Errorf("queryTimeout %v: want error, got nil", qt)
		}
	}
}

func TestNormalizeDSNForcesUnsafeFlagsBack(t *testing.T) {
	raw := "u:pw@tcp(h:3306)/db?multiStatements=true&allowAllFiles=true&allowCleartextPasswords=true&allowOldPasswords=true&interpolateParams=true"
	out, warns := mustNormalize(t, raw, 30*time.Second)
	cfg := mustParse(t, out)

	if cfg.MultiStatements || cfg.AllowAllFiles || cfg.AllowCleartextPasswords || cfg.AllowOldPasswords || cfg.InterpolateParams {
		t.Errorf("unsafe flag survived normalization: %+v", cfg)
	}
	if len(warns) != 5 {
		t.Fatalf("warnings = %v, want 5", warns)
	}
	for _, key := range []string{"multiStatements", "allowAllFiles", "allowCleartextPasswords", "allowOldPasswords", "interpolateParams"} {
		found := false
		for _, w := range warns {
			if strings.Contains(w, key) {
				found = true
			}
			if strings.Contains(w, "pw") {
				t.Errorf("warning leaks password: %q", w)
			}
		}
		if !found {
			t.Errorf("no warning for %s in %v", key, warns)
		}
	}
}

func TestNormalizeDSNExplicitSafeFlagsNoWarning(t *testing.T) {
	_, warns := mustNormalize(t, "u:pw@tcp(h:3306)/db?multiStatements=false&allowAllFiles=false", 30*time.Second)
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
}

func TestNormalizeDSNOverridesTimeoutsAndParseTime(t *testing.T) {
	out, _ := mustNormalize(t, "u:pw@tcp(h:3306)/db?timeout=1h&readTimeout=1h&writeTimeout=1h&parseTime=false", 30*time.Second)
	cfg := mustParse(t, out)
	if cfg.Timeout != 10*time.Second || cfg.ReadTimeout != 40*time.Second || cfg.WriteTimeout != 10*time.Second || !cfg.ParseTime {
		t.Errorf("not overridden: %v %v %v parseTime=%v", cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout, cfg.ParseTime)
	}
}

func TestNormalizeDSNRejectsUnsupportedParams(t *testing.T) {
	tests := []struct {
		name, query string
		key         string // key expected in the error
		secret      string // value that must not appear in the error
	}{
		{"sql_mode", "sql_mode=ANSI_QUOTES", "sql_mode", "ANSI_QUOTES"},
		{"charset", "charset=gbk", "charset", "gbk"},
		{"charset list", "charset=utf8mb4,gbk", "charset", "gbk"},
		{"charset repeated", "charset=utf8mb4&charset=gbk", "charset", "gbk"},
		{"transaction_read_only", "transaction_read_only=0", "transaction_read_only", "=0"},
		{"max_execution_time", "max_execution_time=0", "max_execution_time", "=0"},
		{"unknown", "foo=bar", "foo", "bar"},
		{"known driver option", "allowFallbackToPlaintext=true", "allowFallbackToPlaintext", "=true"},
		{"collation not utf8mb4", "collation=gbk_chinese_ci", "collation", "gbk_chinese_ci"},
		{"wrong case of known key", "multistatements=true", "multistatements", "=true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := normalizeDSN("u:pw@tcp(h:3306)/db?"+tc.query, 30*time.Second)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			want := `unsupported DSN parameter "` + tc.key + `"`
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
			if strings.Contains(err.Error(), tc.secret) || strings.Contains(err.Error(), "pw") {
				t.Errorf("error leaks a value: %q", err)
			}
		})
	}
}

func TestNormalizeDSNKeepsTLSLocCollation(t *testing.T) {
	out, _ := mustNormalize(t, "u:pw@tcp(h:3306)/db?tls=true&loc=UTC&collation=utf8mb4_bin", 30*time.Second)
	cfg := mustParse(t, out)
	if cfg.TLSConfig != "true" {
		t.Errorf("TLSConfig = %q, want true", cfg.TLSConfig)
	}
	if cfg.Loc.String() != "UTC" {
		t.Errorf("Loc = %v, want UTC", cfg.Loc)
	}
	if cfg.Collation != "utf8mb4_bin" {
		t.Errorf("Collation = %q, want utf8mb4_bin", cfg.Collation)
	}
	if got := queryOf(t, out)["charset"]; !reflect.DeepEqual(got, []string{"utf8mb4"}) {
		t.Errorf("charset = %v, want [utf8mb4]", got)
	}
}

func TestNormalizeDSNKeepsNonUTCLoc(t *testing.T) {
	out, _ := mustNormalize(t, "u:pw@tcp(h:3306)/db?loc=Europe%2FBerlin", 30*time.Second)
	if got := mustParse(t, out).Loc.String(); got != "Europe/Berlin" {
		t.Errorf("Loc = %q, want Europe/Berlin", got)
	}
}

func TestNormalizeDSNAcceptsUserCharsetUTF8MB4(t *testing.T) {
	out, _ := mustNormalize(t, "u:pw@tcp(h:3306)/db?charset=utf8mb4", 30*time.Second)
	if got := queryOf(t, out)["charset"]; !reflect.DeepEqual(got, []string{"utf8mb4"}) {
		t.Errorf("charset = %v, want [utf8mb4]", got)
	}
}

func TestNormalizeDSNInvalidDoesNotLeakPassword(t *testing.T) {
	_, _, err := normalizeDSN("u:secretpw@tcp(h:3306", 30*time.Second)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), "secretpw") {
		t.Errorf("error leaks password: %q", err)
	}
}

func TestLoadNormalizesSourceDSNs(t *testing.T) {
	cfg, err := Load(baseEnv("MYSQL_DSN_PROD="+goodDSN+"?multiStatements=true", "QUERY_TIMEOUT=2s"))
	if err != nil {
		t.Fatal(err)
	}
	got := mustParse(t, cfg.Sources[0].DSN)
	if got.Params["max_execution_time"] != "2000" || got.MultiStatements {
		t.Errorf("source DSN not normalized: %q", cfg.Sources[0].DSN)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "prod") || !strings.Contains(cfg.Warnings[0], "multiStatements") {
		t.Errorf("Warnings = %v, want one naming source prod and multiStatements", cfg.Warnings)
	}
}

func TestLoadDSNErrorNamesVarWithoutLeaking(t *testing.T) {
	_, err := Load(baseEnv("MYSQL_DSN_BAD=u:secretpw@tcp(h:3306)/db?sql_mode=ANSI_QUOTES"))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "MYSQL_DSN_BAD") || !strings.Contains(err.Error(), `unsupported DSN parameter "sql_mode"`) {
		t.Errorf("error = %q", err)
	}
	if strings.Contains(err.Error(), "secretpw") || strings.Contains(err.Error(), "ANSI_QUOTES") {
		t.Errorf("error leaks a value: %q", err)
	}
}

func TestSourceRedacted(t *testing.T) {
	tests := []struct {
		name, dsn, want string
	}{
		{"typical", "u:secret@tcp(h:3306)/db?parseTime=true&timeout=10s", "u@tcp(h:3306)/db"},
		{"no db", "u:secret@tcp(h:3306)/", "u@tcp(h:3306)/"},
		{"password with slash and at", "u:s3/cr@t@tcp(h:3306)/db", "u@tcp(h:3306)/db"},
		{"no password", "u@tcp(h:3306)/db", "u@tcp(h:3306)/db"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Source{Name: "x", DSN: tc.dsn}.Redacted()
			if got != tc.want {
				t.Errorf("Redacted() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "secret") || strings.Contains(got, "s3/cr") {
				t.Errorf("Redacted() leaks the password: %q", got)
			}
		})
	}
}

func TestSourceRedactedNormalizedDSN(t *testing.T) {
	cfg, err := Load(baseEnv("MYSQL_DSN_PROD=u:secret@tcp(h:3306)/db"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sources[0].Redacted(); got != "u@tcp(h:3306)/db" {
		t.Errorf("Redacted() = %q", got)
	}
}

func TestSourceRedactedUnparseableDoesNotLeak(t *testing.T) {
	got := Source{Name: "x", DSN: "u:secret@tcp(h:3306"}.Redacted()
	if strings.Contains(got, "secret") {
		t.Errorf("Redacted() leaks the password: %q", got)
	}
}

func TestNormalizeDSNErrorsDoNotLeakSecrets(t *testing.T) {
	tests := []struct{ name, dsn string }{
		{"no slash, password as network", "user:secretpw/db"},
		{"slash inside password", "user:pasecretss/x@tcp(h:3306)"},
		{"bad bool value", "u:pw@tcp(h:3306)/db?parseTime=zzsecret"},
		{"bad duration", "u:pw@tcp(h:3306)/db?timeout=zzsecret"},
		{"bad read timeout", "u:pw@tcp(h:3306)/db?readTimeout=zzsecret"},
		{"bad loc", "u:pw@tcp(h:3306)/db?loc=zzsecret"},
		{"bad loc escape", "u:pw@tcp(h:3306)/db?loc=%zzsecret"},
		{"bad tls escape", "u:pw@tcp(h:3306)/db?tls=%zzsecret"},
		{"unknown tls name", "u:pw@tcp(h:3306)/db?tls=zzsecret"},
		{"bad dbname escape", "u:pw@tcp(h:3306)/%zzsecret"},
		{"password fragment as key", "u:pa/ss?zzsecret=1@tcp(h:3306)"},
		{"unbalanced addr", "u:pw@tcp(zzsecret/db"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := normalizeDSN(tc.dsn, 30*time.Second)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "pw") {
				t.Errorf("error leaks input: %q", err)
			}
		})
	}
}

func TestNormalizeDSNStrictDoesNotPanic(t *testing.T) {
	// The driver panics on strict=...; it must be rejected before parsing.
	_, _, err := normalizeDSN("u:pw@tcp(h:3306)/db?strict=true", 30*time.Second)
	if err == nil || err.Error() != `unsupported DSN parameter "strict"` {
		t.Errorf("error = %v, want unsupported DSN parameter \"strict\"", err)
	}
}

func TestNormalizeDSNRejectsCollationInjection(t *testing.T) {
	for _, c := range []string{
		"utf8mb4_bin,%20sql_mode=''",
		"utf8mb4_bin, sql_mode=''",
		"utf8mb4_bin;x",
		"utf8mb4_",
		"utf8mb4_bin%20x",
	} {
		_, _, err := normalizeDSN("u:pw@tcp(h:3306)/db?collation="+c, 30*time.Second)
		if err == nil {
			t.Errorf("collation %q: want error, got nil", c)
			continue
		}
		if strings.Contains(err.Error(), "sql_mode") {
			t.Errorf("collation %q: error leaks value: %q", c, err)
		}
	}
	for _, c := range []string{"utf8mb4_bin", "utf8mb4_0900_ai_ci", "UTF8MB4_BIN"} {
		if _, _, err := normalizeDSN("u:pw@tcp(h:3306)/db?collation="+c, 30*time.Second); err != nil {
			t.Errorf("collation %q: %v", c, err)
		}
	}
}

func TestNormalizeDSNRejectsObfuscatedKeys(t *testing.T) {
	for _, q := range []string{"%74ls=1", "=x", "t%6Cs=skip-verify", "tls%00=true", "&&=&"} {
		if _, _, err := normalizeDSN("u:pw@tcp(h:3306)/db?"+q, 30*time.Second); err == nil {
			t.Errorf("query %q: want error, got nil", q)
		}
	}
}

func TestNormalizeDSNQueryInDBNameStaysDBName(t *testing.T) {
	out, _ := mustNormalize(t, "u:pw@tcp(h:3306)/db%3Fmultistatements%3Dtrue%26tls%3Dskip-verify", 30*time.Second)
	cfg := mustParse(t, out)
	if cfg.DBName != "db?multistatements=true&tls=skip-verify" {
		t.Errorf("DBName = %q", cfg.DBName)
	}
	if cfg.MultiStatements || cfg.TLSConfig != "" || len(cfg.Params) != 3 {
		t.Errorf("db name was re-interpreted as parameters: %q", out)
	}
}

func TestNormalizeDSNTLSWarnings(t *testing.T) {
	for _, v := range []string{"skip-verify", "preferred"} {
		_, warns := mustNormalize(t, "u:pw@tcp(h:3306)/db?tls="+v, 30*time.Second)
		if len(warns) != 1 || !strings.Contains(warns[0], "tls") {
			t.Errorf("tls=%s: warnings = %v, want one naming tls", v, warns)
		}
	}
	for _, v := range []string{"true", "false"} {
		if _, warns := mustNormalize(t, "u:pw@tcp(h:3306)/db?tls="+v, 30*time.Second); len(warns) != 0 {
			t.Errorf("tls=%s: warnings = %v, want none", v, warns)
		}
	}
}

// outputKeys are the only query keys a normalized DSN may contain.
var outputKeys = map[string]bool{
	"charset": true, "collation": true, "loc": true, "tls": true,
	"parseTime": true, "timeout": true, "readTimeout": true, "writeTimeout": true,
	"transaction_read_only": true, "max_execution_time": true, "sql_mode": true,
}

// checkNormalized asserts the invariants every accepted DSN must satisfy.
func checkNormalized(t *testing.T, in, out string, qt time.Duration) {
	t.Helper()
	cfg, err := mysql.ParseDSN(out)
	if err != nil {
		t.Fatalf("output of %q does not parse: %v", in, err)
	}
	if cfg.MultiStatements || cfg.AllowAllFiles || cfg.AllowCleartextPasswords || cfg.AllowOldPasswords || cfg.InterpolateParams {
		t.Errorf("unsafe flag set in output of %q", in)
	}
	want := map[string]string{
		"transaction_read_only": "1",
		"max_execution_time":    strconv.FormatInt(qt.Milliseconds(), 10),
		"sql_mode":              wantSQLMode,
	}
	if !reflect.DeepEqual(cfg.Params, want) {
		t.Errorf("Params for %q = %v, want %v", in, cfg.Params, want)
	}
	if !cfg.ParseTime {
		t.Errorf("ParseTime false for %q", in)
	}
	vals, err := url.ParseQuery(driverQuery(out))
	if err != nil {
		t.Fatalf("output query of %q: %v", in, err)
	}
	for k := range vals {
		if !outputKeys[k] {
			t.Errorf("output of %q has unexpected key %q: %q", in, k, out)
		}
	}
	if got := vals["charset"]; !reflect.DeepEqual(got, []string{"utf8mb4"}) {
		t.Errorf("charset for %q = %v", in, got)
	}
}

func TestNormalizeDSNOutputInvariants(t *testing.T) {
	inputs := []string{
		"u:pw@tcp(h:3306)/db",
		"u@tcp(h:3306)/",
		"u:p%40ss@tcp(h:3306)/db",
		"u:p/ss?x@tcp(h:3306)/db",
		"u:pw@tcp(h:3306)/db?multiStatements=true&allowAllFiles=true&allowCleartextPasswords=true&allowOldPasswords=true&interpolateParams=true",
		"u:pw@tcp(h:3306)/db?tls=skip-verify&loc=UTC&collation=utf8mb4_bin&charset=utf8mb4",
		"u:pw@tcp(h:3306)/db?timeout=1h&readTimeout=1h&writeTimeout=1h&parseTime=false",
		"u:pw@tcp(h:3306)/db%3Fmultistatements%3Dtrue",
		"u:pw@unix(/tmp/x.sock)/db",
		"u:pw@tcp(h:3306)/db?bareflag",
	}
	for _, in := range inputs {
		out, _ := mustNormalize(t, in, 7*time.Second)
		checkNormalized(t, in, out, 7*time.Second)
	}
}

func FuzzNormalizeDSN(f *testing.F) {
	for _, s := range []string{
		"u:pw@tcp(h:3306)/db",
		"u:pw@tcp(h:3306)/db?multiStatements=true&tls=skip-verify&collation=utf8mb4_bin",
		"u:pw@tcp(h:3306)/db?sql_mode=ANSI_QUOTES&charset=gbk&strict=true",
		"u:pw@tcp(h:3306)/db%3Fmultistatements%3Dtrue",
		"user:secretpw/db", "?=&", "/", "@/", "u:p/ss?x@tcp(h)/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, _, err := normalizeDSN(in, 30*time.Second)
		if err != nil {
			return
		}
		checkNormalized(t, in, out, 30*time.Second)
	})
}

func TestNormalizeDSNExported(t *testing.T) {
	want, wantWarns := mustNormalize(t, "u:pw@tcp(h:3306)/db?multiStatements=true", 5*time.Second)
	got, gotWarns, err := NormalizeDSN("u:pw@tcp(h:3306)/db?multiStatements=true", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || !reflect.DeepEqual(gotWarns, wantWarns) {
		t.Errorf("NormalizeDSN = %q %v, want %q %v", got, gotWarns, want, wantWarns)
	}
}
