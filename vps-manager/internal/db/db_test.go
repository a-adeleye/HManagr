package db

import (
	"reflect"
	"testing"
)

func TestSplitTSV(t *testing.T) {
	got := splitTSV("a\tb\\tc\tNULL\tline\\nbreak\tback\\\\slash")
	want := []string{"a", "b\tc", "NULL", "line\nbreak", "back\\slash"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestParseGridPostgresNulls(t *testing.T) {
	out := "id,name\n1," + nullSentinel + "\n2,\"hi, there\"\n"
	qr, err := parseGrid("postgres", out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(qr.Columns, []string{"id", "name"}) {
		t.Fatalf("columns: %#v", qr.Columns)
	}
	if qr.Rows[0][1] != nil {
		t.Fatalf("expected NULL, got %#v", qr.Rows[0][1])
	}
	if qr.Rows[1][1] != "hi, there" {
		t.Fatalf("expected quoted csv value, got %#v", qr.Rows[1][1])
	}
}

func TestParseGridMySQL(t *testing.T) {
	out := "id\tname\n1\tNULL\n2\ttab\\there\n"
	qr, err := parseGrid("mysql", out)
	if err != nil {
		t.Fatal(err)
	}
	if qr.Rows[0][1] != nil {
		t.Fatalf("expected NULL, got %#v", qr.Rows[0][1])
	}
	if qr.Rows[1][1] != "tab\there" {
		t.Fatalf("expected unescaped tab, got %#v", qr.Rows[1][1])
	}
}

func TestReturnsRows(t *testing.T) {
	cases := map[string]bool{
		"SELECT * FROM t":                     true,
		"  with x as (select 1) select * from x": true,
		"SHOW DATABASES":                      true,
		"UPDATE t SET a=1":                    false,
		"INSERT INTO t VALUES (1) RETURNING id": true,
		"DELETE FROM t WHERE id=1":            false,
	}
	for sql, want := range cases {
		if got := returnsRows(sql); got != want {
			t.Errorf("returnsRows(%q) = %v, want %v", sql, got, want)
		}
	}
}

func TestSQLValEscaping(t *testing.T) {
	v := `it's a \path`
	if got := sqlVal("postgres", &v); got != `'it''s a \path'` {
		t.Errorf("postgres: %s", got)
	}
	if got := sqlVal("mysql", &v); got != `'it''s a \\path'` {
		t.Errorf("mysql: %s", got)
	}
	if got := sqlVal("postgres", nil); got != "NULL" {
		t.Errorf("nil: %s", got)
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := qualIdent("postgres", "public", `we"ird`); got != `"public"."we""ird"` {
		t.Errorf("postgres: %s", got)
	}
	if got := qualIdent("mysql", "appdb", "users"); got != "`users`" {
		t.Errorf("mysql: %s", got)
	}
}
