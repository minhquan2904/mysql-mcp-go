package guard

import "strings"

// Case is a statement that Check must reject, with a substring (matched
// case-insensitively) that the rejection reason must contain.
type Case struct{ SQL, Reason string }

// AllowCases must all pass Check. Exported so integration tests can reuse them.
var AllowCases = []string{
	"SELECT 1",
	"  select * from t;",
	"(SELECT 1) UNION (SELECT 2)",
	"SHOW TABLES",
	"SHOW CREATE TABLE orders",
	"DESC orders",
	"DESCRIBE orders",
	"EXPLAIN SELECT * FROM t",
	"EXPLAIN FORMAT=JSON SELECT 1",
	"EXPLAIN ANALYZE SELECT 1",
	"WITH c AS (SELECT 1) SELECT * FROM c",
	"SELECT 'DROP TABLE x; DELETE' AS s",
	"SELECT update_at, deleted FROM t",
	"SELECT SLEEP(1)",
	"SELECT 2--1",
	"SELECT 1 -- trailing comment",
	"SELECT `get_lock_col` FROM t",
	"SELECT * FROM t WHERE a = 1",
	"SHOW COLUMNS FROM orders",
	"SHOW INDEX FROM orders",
	"SHOW TABLE STATUS LIKE 'o%'",
	"SELECT 'processlist'",
	"SELECT keyring_col, service_name FROM t",
}

// RejectCases must all fail Check with an error wrapping ErrNotReadOnly.
// Exported so integration tests can reuse them.
var RejectCases = []Case{
	{"INSERT INTO t VALUES (1)", "INSERT"},
	{"UPDATE t SET a=1", "UPDATE"},
	{"DELETE FROM t", "DELETE"},
	{"DROP TABLE t", "DROP"},
	{"CREATE TEMPORARY TABLE t (i int)", "CREATE"},
	{"SET @a = 1", "SET"},
	{"CALL p()", "CALL"},
	{"HANDLER t OPEN", "HANDLER"},
	{"TABLE orders", "TABLE"},
	{"VALUES ROW(1)", "VALUES"},
	{"SELECT 1; DROP TABLE x", "multiple statements"},
	{"/* hi */ DELETE FROM t", "DELETE"},
	{"-- c\nDELETE FROM t", "DELETE"},
	{"/*!50000 DROP TABLE t */", "versioned"},
	{"SELECT 1 /*+ BKA(t) */", "optimizer"},
	{"SELECT * FROM t INTO OUTFILE '/tmp/x'", "OUTFILE"},
	{"SELECT * INTO DUMPFILE '/tmp/x' FROM t", "DUMPFILE"},
	{"SELECT 1 INTO @v", "INTO @"},
	{"SELECT * FROM t FOR UPDATE", "FOR UPDATE"},
	{"SELECT * FROM t FOR SHARE", "FOR SHARE"},
	{"SELECT * FROM t LOCK IN SHARE MODE", "LOCK IN SHARE MODE"},
	{"WITH c AS (SELECT 1) UPDATE t SET a=1", "UPDATE"},
	{"WITH c AS (SELECT 1) DELETE FROM t", "DELETE"},
	{"EXPLAIN DELETE FROM t", "DELETE"},
	{"EXPLAIN ANALYZE UPDATE t SET a=1", "UPDATE"},
	{"DESC ANALYZE DELETE FROM t", "DELETE"},
	{"SELECT GET_LOCK('x', 10)", "GET_LOCK"},
	{"SELECT `GET_LOCK`('x', 10)", "GET_LOCK"},
	{"SELECT get_lock ('x', 10)", "GET_LOCK"},
	{"SELECT LOAD_FILE('/etc/passwd')", "LOAD_FILE"},
	{"SELECT BENCHMARK(1e9, MD5('a'))", "BENCHMARK"},
	{"SELECT @x := (SELECT 1)", "assignment"},
	{"SELECT 1 --1, GET_LOCK('x',1)", "GET_LOCK"},
	{"SELECT 1 --1 INTO OUTFILE '/tmp/x'", "OUTFILE"},
	{"SELECT `a\\` , GET_LOCK('app',1), `b`", "GET_LOCK"},
	// Literals in backslash-escape mode, but GET_LOCK is exposed when the
	// server runs with NO_BACKSLASH_ESCAPES.
	{`SELECT 'a\' , GET_LOCK('x',1), '`, "GET_LOCK"},
	{`SELECT "a\" , GET_LOCK('x',1), "`, "GET_LOCK"},
	{"", "empty"},
	{"   ", "empty"},
	{";", "empty"},
	{"SELECT 'unterminated", "unterminated"},
	{"SELECT \xff", "UTF-8"},
	{strings.Repeat("a", MaxSQLBytes+1), "too long"},

	// Control bytes: MySQL treats some as whitespace/comment delimiters.
	{"SELECT 1 --\f GET_LOCK('x',1)", "control character not allowed"},
	{"SELECT 1 --\v1", "control character not allowed"},
	{"SELECT 1\x00", "control character not allowed"},
	{"SELECT\x0b1", "control character not allowed"},
	{"SELECT 1\x7f", "control character not allowed"},

	// Server-internal function families (matched by prefix).
	{"SELECT VERSION_TOKENS_LOCK_SHARED('a', 1)", "VERSION_TOKENS_LOCK_SHARED"},
	{"SELECT KEYRING_KEY_FETCH('k')", "KEYRING_KEY_FETCH"},
	{"SELECT `keyring_key_fetch` ('k')", "KEYRING_KEY_FETCH"},
	{"SELECT AUDIT_LOG_READ()", "AUDIT_LOG_READ"},
	{"SELECT GROUP_REPLICATION_SET_AS_PRIMARY('x')", "GROUP_REPLICATION_SET_AS_PRIMARY"},
	{"SELECT ASYNCHRONOUS_CONNECTION_FAILOVER_ADD_SOURCE('c')", "ASYNCHRONOUS_CONNECTION_FAILOVER_ADD_SOURCE"},
	{"SELECT SERVICE_FOO()", "SERVICE_FOO"},

	// SHOW is allowlisted by object.
	{"SHOW PROCESSLIST", "SHOW PROCESSLIST not allowed"},
	{"SHOW FULL PROCESSLIST", "SHOW FULL PROCESSLIST not allowed"},
	{"SHOW ENGINE INNODB STATUS", "SHOW ENGINE not allowed"},
	{"SHOW GRANTS FOR root", "SHOW GRANTS not allowed"},
	{"SHOW BINARY LOGS", "SHOW BINARY not allowed"},
	{"SHOW REPLICA STATUS", "SHOW REPLICA not allowed"},
	{"SHOW VARIABLES", "SHOW VARIABLES not allowed"},
	{"SHOW CREATE USER root", "SHOW CREATE USER not allowed"},

	// Objects that leak statement text; list_active_queries redacts them.
	{"SELECT INFO FROM information_schema.PROCESSLIST", "access to PROCESSLIST not allowed"},
	{"SELECT * FROM `information_schema`.`processlist`", "access to PROCESSLIST not allowed"},
	{"SELECT trx_query FROM information_schema.INNODB_TRX", "access to TRX_QUERY not allowed"},
	{"SELECT SQL_TEXT FROM performance_schema.events_statements_history", "access to SQL_TEXT not allowed"},
	{"SELECT PROCESSLIST_INFO FROM performance_schema.threads", "access to PROCESSLIST_INFO not allowed"},
}
