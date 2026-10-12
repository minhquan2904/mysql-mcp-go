package source

import (
	"strings"
	"testing"
	"time"
)

const tou = " TO `mcp_ro`@`%`"

func TestCheckGrantsAllowed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		grants []string
	}{
		{"nothing", nil},
		{"mcp_ro shape (no USAGE line, as on 8.4)", []string{
			"GRANT PROCESS ON *.*" + tou,
			"GRANT SELECT, SHOW VIEW ON `testdb`.*" + tou,
			"GRANT SELECT ON `performance_schema`.`data_locks`" + tou,
		}},
		{"usage plus schema select", []string{
			"GRANT USAGE ON *.*" + tou,
			"GRANT SELECT ON testdb.*" + tou,
		}},
		{"table level select", []string{
			"GRANT USAGE ON *.*" + tou,
			"GRANT SELECT ON `testdb`.`orders`" + tou,
		}},
		{"column level select", []string{
			"GRANT USAGE ON *.*" + tou,
			"GRANT SELECT (`id`, `email`) ON `testdb`.`customers`" + tou,
		}},
		{"quoted user and host", []string{
			"GRANT USAGE ON *.* TO 'mcp ro'@'10.%'",
			"GRANT SELECT ON `testdb`.* TO 'mcp ro'@'10.%'",
		}},
		{"lowercase keywords", []string{"grant select on `testdb`.* to `u`@`h`"}},
		{"escaped wildcard schema", []string{"GRANT SELECT ON `app\\_%`.*" + tou}},
		{"backtick in identifier", []string{"GRANT SELECT ON `te``st`.*" + tou}},
		{"identifier containing keywords", []string{"GRANT SELECT ON `to on`.`with grant option`" + tou}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckGrants(tc.grants); err != nil {
				t.Errorf("CheckGrants = %v, want nil", err)
			}
		})
	}
}

func TestCheckGrantsRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		grants []string
		want   string // substring of the error
	}{
		{"global select", []string{"GRANT SELECT ON *.*" + tou}, "global SELECT"},
		{"global select among others", []string{"GRANT PROCESS, SELECT ON *.*" + tou}, "global SELECT"},
		{"global show view", []string{"GRANT SHOW VIEW ON *.*" + tou}, "global SHOW VIEW"},
		{"select on mysql schema", []string{"GRANT SELECT ON `mysql`.*" + tou}, "mysql"},
		{"select on mysql table", []string{"GRANT SELECT ON `mysql`.`user`" + tou}, "mysql"},
		{"select on sys schema", []string{"GRANT SELECT ON `sys`.*" + tou}, "sys"},
		{"select on MySQL mixed case", []string{"GRANT SELECT ON `MySQL`.*" + tou}, "MySQL"},
		{"wildcard schema covers mysql", []string{"GRANT SELECT ON `%`.*" + tou}, "mysql"},
		{"underscore wildcard covers mysql", []string{"GRANT SELECT ON `my_ql`.*" + tou}, "mysql"},
		{"prefix wildcard covers sys", []string{"GRANT SELECT ON `s%`.*" + tou}, "sys"},
		{"whole performance_schema", []string{"GRANT SELECT ON `performance_schema`.*" + tou}, "performance_schema"},
		{"all privileges", []string{"GRANT ALL PRIVILEGES ON *.*" + tou}, "ALL PRIVILEGES"},
		{"all privileges on schema", []string{"GRANT ALL PRIVILEGES ON `testdb`.*" + tou}, "ALL PRIVILEGES"},
		{"dynamic admin privilege", []string{"GRANT SYSTEM_VARIABLES_ADMIN ON *.*" + tou}, "SYSTEM_VARIABLES_ADMIN"},
		{"dynamic admin among others", []string{"GRANT PROCESS, BINLOG_ADMIN, SELECT ON `testdb`.*" + tou}, "BINLOG_ADMIN"},
		{"with grant option", []string{"GRANT SELECT ON `testdb`.* TO `mcp_ro`@`%` WITH GRANT OPTION"}, "GRANT OPTION"},
		{"usage with grant option", []string{"GRANT USAGE ON *.* TO `u`@`%` WITH GRANT OPTION"}, "GRANT OPTION"},
		{"role grant", []string{"GRANT `role`@`%` TO `mcp_ro`@`%`"}, "roles not supported"},
		{"role grant unquoted host", []string{"GRANT `r1`, `r2` TO `u`@`%`"}, "roles not supported"},
		{"unparseable line fails closed", []string{"SELECT 1"}, "unrecognized"},
		{"revoke line fails closed", []string{"REVOKE SELECT ON `mysql`.* FROM `u`@`%`"}, "unrecognized"},
		{"object without schema fails closed", []string{"GRANT SELECT ON orders" + tou}, "unrecognized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckGrants(tc.grants)
			if err == nil {
				t.Fatal("CheckGrants = nil, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestCheckGrantsNamesForbiddenPrivilege(t *testing.T) {
	for _, priv := range []string{
		"INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER", "INDEX", "TRIGGER", "EVENT",
		"EXECUTE", "FILE", "SUPER", "CREATE VIEW", "CREATE ROUTINE", "ALTER ROUTINE",
		"CREATE USER", "RELOAD", "SHUTDOWN", "LOCK TABLES", "CREATE TEMPORARY TABLES",
		"REFERENCES", "REPLICATION SLAVE", "GRANT OPTION",
	} {
		for _, obj := range []string{"`testdb`.*", "*.*", "`testdb`.`orders`"} {
			err := CheckGrants([]string{"GRANT SELECT, " + priv + " ON " + obj + tou})
			if err == nil {
				t.Errorf("%s ON %s: nil, want error", priv, obj)
			} else if !strings.Contains(err.Error(), priv) {
				t.Errorf("%s ON %s: error %q does not name the privilege", priv, obj, err)
			}
		}
	}
}

func TestCheckGrantsReportsEveryProblem(t *testing.T) {
	err := CheckGrants([]string{
		"GRANT INSERT ON `testdb`.*" + tou,
		"GRANT SELECT ON *.*" + tou,
	})
	if err == nil || !strings.Contains(err.Error(), "INSERT") || !strings.Contains(err.Error(), "global SELECT") {
		t.Errorf("error = %v, want both INSERT and global SELECT", err)
	}
}

func TestCheckGrantsDeduplicatesProblems(t *testing.T) {
	err := CheckGrants([]string{
		"GRANT SELECT ON *.* TO `a`@`%`",
		"GRANT SELECT ON *.* TO `b`@`%`",
	})
	if err == nil {
		t.Fatal("want error")
	}
	if n := strings.Count(err.Error(), "global SELECT"); n != 1 {
		t.Errorf("%q mentions global SELECT %d times, want 1", err, n)
	}
}

func TestCheckGrantsSensitivePerformanceSchemaTables(t *testing.T) {
	for _, table := range []string{
		"events_statements_current", "events_statements_history", "events_statements_history_long",
		"user_variables_by_thread", "variables_by_thread", "session_connect_attrs",
		"session_account_connect_attrs", "EVENTS_STATEMENTS_HISTORY",
	} {
		err := CheckGrants([]string{"GRANT SELECT ON `performance_schema`.`" + table + "`" + tou})
		if err == nil || !strings.Contains(err.Error(), table) {
			t.Errorf("%s: err = %v, want it rejected by name", table, err)
		}
	}
	// a wildcard schema that reaches performance_schema is no way around it
	if err := CheckGrants([]string{"GRANT SELECT ON `perf%`.`variables_by_thread`" + tou}); err == nil {
		t.Error("wildcard schema over a sensitive table accepted")
	}
	// column-level SELECT is still SELECT
	if err := CheckGrants([]string{"GRANT SELECT (`THREAD_ID`) ON `performance_schema`.`session_connect_attrs`" + tou}); err == nil {
		t.Error("column-level SELECT on a sensitive table accepted")
	}
	// tables the phase 6 tools read stay allowed, as do same-named tables elsewhere
	for _, table := range []string{
		"events_statements_summary_by_digest", "data_locks", "data_lock_waits", "threads",
		"table_io_waits_summary_by_table",
	} {
		if err := CheckGrants([]string{"GRANT SELECT ON `performance_schema`.`" + table + "`" + tou}); err != nil {
			t.Errorf("%s: %v, want nil", table, err)
		}
	}
	if err := CheckGrants([]string{"GRANT SELECT ON `testdb`.`variables_by_thread`" + tou}); err != nil {
		t.Errorf("same table name in another schema: %v, want nil", err)
	}
}

func TestCheckGrantsOtherShapes(t *testing.T) {
	ok := []struct{ name, line string }{
		{"tabs and newlines between words", "GRANT\tSELECT,\n SHOW VIEW\tON\n`testdb`.*\tTO\n`u`@`%`"},
		{"AS ... WITH ROLE suffix", "GRANT SELECT ON `testdb`.* TO `u`@`%` AS `root`@`%` WITH ROLE `r`"},
		{"usage only", "GRANT USAGE ON *.* TO `u`@`%`"},
	}
	for _, tc := range ok {
		if err := CheckGrants([]string{tc.line}); err != nil {
			t.Errorf("%s: %v, want nil", tc.name, err)
		}
	}

	bad := []struct{ name, line, want string }{
		{"proxy", "GRANT PROXY ON ''@'' TO `root`@`localhost` WITH GRANT OPTION", ""},
		{"grant option with tab", "GRANT SELECT ON `testdb`.* TO `u`@`%` WITH\tGRANT\nOPTION", "GRANT OPTION"},
		{"grant option before AS", "GRANT SELECT ON `testdb`.* TO `u`@`%` WITH GRANT OPTION AS `root`@`%` WITH ROLE `r`", "GRANT OPTION"},
		{"tab separated insert", "GRANT\tINSERT\tON\t`testdb`.*\tTO\t`u`@`%`", "INSERT"},
	}
	for _, tc := range bad {
		err := CheckGrants([]string{tc.line})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want error containing %q", tc.name, err, tc.want)
		}
	}

	// a REVOKE line next to valid grants still fails closed, and the valid
	// lines do not hide it
	err := CheckGrants([]string{
		"GRANT USAGE ON *.*" + tou,
		"GRANT SELECT ON `testdb`.*" + tou,
		"REVOKE SELECT ON `mysql`.* FROM `mcp_ro`@`%`",
	})
	if err == nil || !strings.Contains(err.Error(), "grant 3: unrecognized") {
		t.Errorf("err = %v, want grant 3 unrecognized", err)
	}
}

func TestSchemaMatchesIsNotExponential(t *testing.T) {
	done := make(chan bool, 1)
	go func() {
		done <- schemaMatches(strings.Repeat("%", 20)+"x", "performance_schema")
	}()
	select {
	case got := <-done:
		if got {
			t.Error("matched, want no match")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("schemaMatches did not finish in 2s (backtracking blow-up)")
	}

	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"%", "mysql", true},
		{"%%%%", "sys", true},
		{"my%", "mysql", true},
		{"%sql", "mysql", true},
		{"m%s%l", "mysql", true},
		{"my_ql", "mysql", true},
		{"my__ql", "mysql", false},
		{`my\_ql`, "mysql", false},
		{`app\_%`, "app_x", true},
		{`app\_%`, "appx", false},
		{"MYSQL", "mysql", true},
		{"s%", "mysql", false},
		{"", "", true},
		{"%x", "performance_schema", false},
	} {
		if got := schemaMatches(tc.pattern, tc.name); got != tc.want {
			t.Errorf("schemaMatches(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}
