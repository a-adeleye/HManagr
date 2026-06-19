// Package db manages databases running inside docker containers on a remote
// VPS. All access goes through `docker exec` with the engine's own CLI client
// (psql / mysql / mariadb), so nothing needs to be installed on the VPS beyond
// docker itself, and no database port has to be exposed or tunneled.
//
// Credentials are sniffed from the container's environment (POSTGRES_USER,
// MYSQL_ROOT_PASSWORD, …), which is how the official images are configured in
// practice.
package db

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	sshpkg "vps-manager/internal/ssh"
)

// ExecFn mirrors migration.ExecFn — the App layer supplies it, optionally
// wrapped with sudo for hosts where docker needs elevation.
type ExecFn func(ctx context.Context, cmd string) (*sshpkg.ExecResult, error)

// Container is a docker container recognized as a database engine.
// Password stays backend-side (json:"-"): the frontend only ever passes the
// container ID back, never credentials.
type Container struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Image     string `json:"image"`
	State     string `json:"state"`
	Engine    string `json:"engine"` // "postgres" or "mysql" (mariadb included)
	User      string `json:"user"`
	DefaultDB string `json:"defaultDb"`
	Password  string `json:"-"`
}

type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	IsPK     bool   `json:"isPk"`
}

// QueryResult is what every read path returns. Row cells are *string wrapped
// in any (so SQL NULL survives the trip to the frontend as JSON null; `any`
// rather than *string keeps the wails TS model generator happy).
type QueryResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Message string   `json:"message,omitempty"` // DML outcome, e.g. "UPDATE 3"
	Total   int64    `json:"total"`             // full table count when browsing; otherwise len(Rows)
}

// nullSentinel marks SQL NULL in psql CSV output (psql prints the null string
// as-is even in CSV mode). Chosen to be vanishingly unlikely in real data.
const nullSentinel = "␀NULL␀"

// ───────── container discovery ─────────

type dockerPSLine struct {
	ID    string `json:"ID"`
	Names string `json:"Names"`
	Image string `json:"Image"`
	State string `json:"State"`
}

// ListContainers finds DB containers (running or not) and, for running ones,
// sniffs credentials from their environment. When filter is non-empty it scopes
// `docker ps` (e.g. to one compose project's working dir).
func ListContainers(ctx context.Context, exec ExecFn, filter string) ([]Container, error) {
	cmd := `docker ps -a --format '{{json .}}'`
	if filter != "" {
		cmd = `docker ps -a --filter ` + shellQuote(filter) + ` --format '{{json .}}'`
	}
	res, err := exec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("docker ps: %s", firstNonEmpty(strings.TrimSpace(res.Stderr), fmt.Sprintf("exit %d", res.ExitCode)))
	}

	var out []Container
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if line == "" {
			continue
		}
		var raw dockerPSLine
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		engine := classifyImage(raw.Image)
		if engine == "" {
			continue
		}
		c := Container{
			ID:     raw.ID,
			Name:   raw.Names,
			Image:  raw.Image,
			State:  strings.ToLower(raw.State),
			Engine: engine,
		}
		if c.State == "running" {
			if creds, err := inspectCreds(ctx, exec, raw.ID, engine); err == nil {
				c.User, c.Password, c.DefaultDB = creds.user, creds.password, creds.defaultDB
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == "running"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func classifyImage(image string) string {
	img := strings.ToLower(image)
	// Strip registry/tag noise is unnecessary — substring match is enough.
	switch {
	case strings.Contains(img, "postgres"), strings.Contains(img, "pgvector"), strings.Contains(img, "timescale"):
		return "postgres"
	case strings.Contains(img, "mysql"), strings.Contains(img, "mariadb"), strings.Contains(img, "percona"):
		return "mysql"
	}
	return ""
}

type creds struct {
	user, password, defaultDB string
}

func inspectCreds(ctx context.Context, exec ExecFn, containerID, engine string) (*creds, error) {
	if !validContainerID(containerID) {
		return nil, fmt.Errorf("invalid container id")
	}
	res, err := exec(ctx, "docker inspect --format '{{json .Config.Env}}' "+containerID)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("docker inspect: %s", strings.TrimSpace(res.Stderr))
	}
	var envs []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &envs); err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}

	env := map[string]string{}
	for _, e := range envs {
		if k, v, ok := strings.Cut(e, "="); ok {
			env[k] = v
		}
	}

	c := &creds{}
	switch engine {
	case "postgres":
		c.user = firstNonEmpty(env["POSTGRES_USER"], "postgres")
		c.password = env["POSTGRES_PASSWORD"]
		c.defaultDB = firstNonEmpty(env["POSTGRES_DB"], c.user)
	case "mysql":
		switch {
		case env["MYSQL_ROOT_PASSWORD"] != "":
			c.user, c.password = "root", env["MYSQL_ROOT_PASSWORD"]
		case env["MARIADB_ROOT_PASSWORD"] != "":
			c.user, c.password = "root", env["MARIADB_ROOT_PASSWORD"]
		case env["MYSQL_USER"] != "":
			c.user, c.password = env["MYSQL_USER"], env["MYSQL_PASSWORD"]
		case env["MARIADB_USER"] != "":
			c.user, c.password = env["MARIADB_USER"], env["MARIADB_PASSWORD"]
		default:
			c.user = "root" // MYSQL_ALLOW_EMPTY_PASSWORD setups
		}
		c.defaultDB = firstNonEmpty(env["MYSQL_DATABASE"], env["MARIADB_DATABASE"])
	}
	return c, nil
}

// ───────── SQL execution ─────────

// runSQL executes one SQL string inside the container via the engine's CLI.
// grid=true asks for machine-parseable tabular output (CSV for psql, TSV for
// mysql); grid=false captures the client's status output for DML.
func runSQL(ctx context.Context, exec ExecFn, c *Container, dbName, sql string, grid bool) (*sshpkg.ExecResult, error) {
	if !validContainerID(c.ID) {
		return nil, fmt.Errorf("invalid container id")
	}
	var cmd string
	switch c.Engine {
	case "postgres":
		db := firstNonEmpty(dbName, c.DefaultDB, "postgres")
		args := "psql -X -v ON_ERROR_STOP=1 -U " + shellQuote(c.User) + " -d " + shellQuote(db)
		if grid {
			args += " --csv -P null=" + shellQuote(nullSentinel)
		}
		args += " -c " + shellQuote(sql)
		cmd = "docker exec -e PGPASSWORD=" + shellQuote(c.Password) + " " + c.ID + " " + args
	case "mysql":
		// Prefer the mariadb binary when present: on modern mariadb images the
		// mysql name is a deprecated symlink that warns on stderr.
		inner := `exec "$(command -v mariadb || command -v mysql)" -u` + shellQuote(c.User)
		if dbName != "" {
			inner += " -D " + shellQuote(dbName)
		}
		if grid {
			inner += " --batch --column-names"
		} else {
			inner += " -vv" // prints "Query OK, N rows affected"
		}
		inner += " -e " + shellQuote(sql)
		cmd = "docker exec -e MYSQL_PWD=" + shellQuote(c.Password) + " " + c.ID + " sh -c " + shellQuote(inner)
	default:
		return nil, fmt.Errorf("unsupported engine %q", c.Engine)
	}
	res, err := exec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" {
			msg = fmt.Sprintf("exit %d", res.ExitCode)
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return res, nil
}

// returnsRows guesses whether a statement produces a result set. Imperfect by
// design — worst case a DML statement gets shown as an empty grid or a SELECT
// as a raw message, never data loss.
func returnsRows(sql string) bool {
	s := strings.ToLower(strings.TrimSpace(sql))
	for _, p := range []string{"select", "with", "show", "explain", "describe", "desc ", "table ", "values"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return strings.Contains(s, "returning")
}

// Query runs arbitrary SQL. Statements that return rows come back as a grid;
// everything else comes back as a status message ("UPDATE 3", "Query OK …").
func Query(ctx context.Context, exec ExecFn, c *Container, dbName, sql string) (*QueryResult, error) {
	if strings.TrimSpace(sql) == "" {
		return nil, fmt.Errorf("empty query")
	}
	grid := returnsRows(sql)
	res, err := runSQL(ctx, exec, c, dbName, sql, grid)
	if err != nil {
		return nil, err
	}
	if grid {
		qr, err := parseGrid(c.Engine, res.Stdout)
		if err != nil {
			return nil, err
		}
		return qr, nil
	}
	msg := dmlMessage(c.Engine, res.Stdout)
	return &QueryResult{Message: msg}, nil
}

// dmlMessage distills the client's chatter into a one-line outcome.
func dmlMessage(engine, stdout string) string {
	out := strings.TrimSpace(stdout)
	if engine == "mysql" {
		// -vv echoes the statement between ---- markers; keep only status lines.
		var keep []string
		for _, line := range strings.Split(out, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "Query OK") || strings.Contains(l, "affected") || strings.HasPrefix(l, "Rows matched") {
				keep = append(keep, l)
			}
		}
		if len(keep) > 0 {
			return strings.Join(keep, " · ")
		}
		return "OK"
	}
	if out == "" {
		return "OK"
	}
	return out
}

// parseGrid turns raw client output into columns + rows.
func parseGrid(engine, stdout string) (*QueryResult, error) {
	qr := &QueryResult{Columns: []string{}, Rows: [][]any{}}
	if strings.TrimSpace(stdout) == "" {
		return qr, nil // statement produced no result set
	}
	switch engine {
	case "postgres":
		r := csv.NewReader(strings.NewReader(stdout))
		r.FieldsPerRecord = -1
		records, err := r.ReadAll()
		if err != nil {
			return nil, fmt.Errorf("parse csv: %w", err)
		}
		if len(records) == 0 {
			return qr, nil
		}
		qr.Columns = records[0]
		for _, rec := range records[1:] {
			row := make([]any, len(rec))
			for i, v := range rec {
				if v == nullSentinel {
					row[i] = nil
				} else {
					row[i] = v
				}
			}
			qr.Rows = append(qr.Rows, row)
		}
	case "mysql":
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		if len(lines) == 0 {
			return qr, nil
		}
		qr.Columns = splitTSV(lines[0])
		for _, line := range lines[1:] {
			fields := splitTSV(line)
			row := make([]any, len(fields))
			for i, v := range fields {
				// mysql --batch prints NULL literally; indistinguishable from a
				// real "NULL" string — a known CLI-transport tradeoff.
				if v == "NULL" {
					row[i] = nil
				} else {
					row[i] = v
				}
			}
			qr.Rows = append(qr.Rows, row)
		}
	}
	qr.Total = int64(len(qr.Rows))
	return qr, nil
}

// splitTSV splits one line of `mysql --batch` output and unescapes \t \n \\ \0.
func splitTSV(line string) []string {
	fields := strings.Split(line, "\t")
	for i, f := range fields {
		if !strings.ContainsRune(f, '\\') {
			continue
		}
		var b strings.Builder
		for j := 0; j < len(f); j++ {
			if f[j] == '\\' && j+1 < len(f) {
				switch f[j+1] {
				case 't':
					b.WriteByte('\t')
				case 'n':
					b.WriteByte('\n')
				case '\\':
					b.WriteByte('\\')
				case '0':
					b.WriteByte(0)
				default:
					b.WriteByte(f[j+1])
				}
				j++
				continue
			}
			b.WriteByte(f[j])
		}
		fields[i] = b.String()
	}
	return fields
}

// ───────── metadata ─────────

func ListDatabases(ctx context.Context, exec ExecFn, c *Container) ([]string, error) {
	var sql string
	switch c.Engine {
	case "postgres":
		sql = "SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY 1"
	case "mysql":
		sql = "SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('information_schema','performance_schema','sys','mysql') ORDER BY 1"
	}
	qr, err := Query(ctx, exec, c, "", sql)
	if err != nil {
		return nil, err
	}
	return firstColumn(qr), nil
}

func ListTables(ctx context.Context, exec ExecFn, c *Container, dbName string) ([]Table, error) {
	var sql string
	switch c.Engine {
	case "postgres":
		sql = "SELECT table_schema, table_name FROM information_schema.tables WHERE table_type='BASE TABLE' AND table_schema NOT IN ('pg_catalog','information_schema') ORDER BY 1,2"
	case "mysql":
		sql = "SELECT table_schema, table_name FROM information_schema.tables WHERE table_schema=" + sqlLit(dbName) + " AND table_type='BASE TABLE' ORDER BY 2"
	}
	qr, err := Query(ctx, exec, c, dbName, sql)
	if err != nil {
		return nil, err
	}
	tables := []Table{}
	for _, row := range qr.Rows {
		schema, okS := cellString(row, 0)
		name, okN := cellString(row, 1)
		if okS && okN {
			tables = append(tables, Table{Schema: schema, Name: name})
		}
	}
	return tables, nil
}

func TableColumns(ctx context.Context, exec ExecFn, c *Container, dbName, schema, table string) ([]Column, error) {
	switch c.Engine {
	case "postgres":
		colSQL := "SELECT column_name, data_type, is_nullable FROM information_schema.columns WHERE table_schema=" +
			sqlLit(schema) + " AND table_name=" + sqlLit(table) + " ORDER BY ordinal_position"
		qr, err := Query(ctx, exec, c, dbName, colSQL)
		if err != nil {
			return nil, err
		}
		pkSQL := "SELECT kcu.column_name FROM information_schema.table_constraints tc " +
			"JOIN information_schema.key_column_usage kcu ON tc.constraint_name=kcu.constraint_name AND tc.table_schema=kcu.table_schema " +
			"WHERE tc.constraint_type='PRIMARY KEY' AND tc.table_schema=" + sqlLit(schema) + " AND tc.table_name=" + sqlLit(table)
		pkQR, err := Query(ctx, exec, c, dbName, pkSQL)
		if err != nil {
			return nil, err
		}
		pk := map[string]bool{}
		for _, n := range firstColumn(pkQR) {
			pk[n] = true
		}
		cols := []Column{}
		for _, row := range qr.Rows {
			name, ok := cellString(row, 0)
			if !ok {
				continue
			}
			typ, _ := cellString(row, 1)
			nullable, _ := cellString(row, 2)
			cols = append(cols, Column{
				Name:     name,
				Type:     typ,
				Nullable: strings.EqualFold(nullable, "YES"),
				IsPK:     pk[name],
			})
		}
		return cols, nil
	case "mysql":
		sql := "SELECT column_name, column_type, is_nullable, column_key FROM information_schema.columns WHERE table_schema=" +
			sqlLit(schema) + " AND table_name=" + sqlLit(table) + " ORDER BY ordinal_position"
		qr, err := Query(ctx, exec, c, dbName, sql)
		if err != nil {
			return nil, err
		}
		cols := []Column{}
		for _, row := range qr.Rows {
			name, ok := cellString(row, 0)
			if !ok {
				continue
			}
			typ, _ := cellString(row, 1)
			nullable, _ := cellString(row, 2)
			key, _ := cellString(row, 3)
			cols = append(cols, Column{
				Name:     name,
				Type:     typ,
				Nullable: strings.EqualFold(nullable, "YES"),
				IsPK:     strings.EqualFold(key, "PRI"),
			})
		}
		return cols, nil
	}
	return nil, fmt.Errorf("unsupported engine %q", c.Engine)
}

// TableRows returns one page of rows plus the total row count.
func TableRows(ctx context.Context, exec ExecFn, c *Container, dbName, schema, table string, limit, offset int) (*QueryResult, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	ident := qualIdent(c.Engine, schema, table)

	countQR, err := Query(ctx, exec, c, dbName, "SELECT COUNT(*) FROM "+ident)
	if err != nil {
		return nil, err
	}
	var total int64
	if vals := firstColumn(countQR); len(vals) > 0 {
		total, _ = strconv.ParseInt(vals[0], 10, 64)
	}

	qr, err := Query(ctx, exec, c, dbName,
		fmt.Sprintf("SELECT * FROM %s LIMIT %d OFFSET %d", ident, limit, offset))
	if err != nil {
		return nil, err
	}
	qr.Total = total
	return qr, nil
}

// ───────── row mutations ─────────
// Values arrive as strings (or nil for NULL) and are sent as quoted literals;
// both engines coerce string literals to the column type, which covers the
// practical cases a row editor needs.

func InsertRow(ctx context.Context, exec ExecFn, c *Container, dbName, schema, table string, values map[string]*string) (string, error) {
	if len(values) == 0 {
		return "", fmt.Errorf("no values to insert")
	}
	cols := sortedKeys(values)
	var names, lits []string
	for _, k := range cols {
		names = append(names, quoteIdent(c.Engine, k))
		lits = append(lits, sqlVal(c.Engine, values[k]))
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		qualIdent(c.Engine, schema, table), strings.Join(names, ", "), strings.Join(lits, ", "))
	return mutate(ctx, exec, c, dbName, sql)
}

func UpdateRow(ctx context.Context, exec ExecFn, c *Container, dbName, schema, table string, pk, values map[string]*string) (string, error) {
	if len(pk) == 0 {
		return "", fmt.Errorf("refusing to update without a primary key")
	}
	if len(values) == 0 {
		return "", fmt.Errorf("no changes to apply")
	}
	var sets []string
	for _, k := range sortedKeys(values) {
		sets = append(sets, quoteIdent(c.Engine, k)+" = "+sqlVal(c.Engine, values[k]))
	}
	sql := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		qualIdent(c.Engine, schema, table), strings.Join(sets, ", "), whereClause(c.Engine, pk))
	return mutate(ctx, exec, c, dbName, sql)
}

func DeleteRow(ctx context.Context, exec ExecFn, c *Container, dbName, schema, table string, pk map[string]*string) (string, error) {
	if len(pk) == 0 {
		return "", fmt.Errorf("refusing to delete without a primary key")
	}
	sql := fmt.Sprintf("DELETE FROM %s WHERE %s",
		qualIdent(c.Engine, schema, table), whereClause(c.Engine, pk))
	return mutate(ctx, exec, c, dbName, sql)
}

func mutate(ctx context.Context, exec ExecFn, c *Container, dbName, sql string) (string, error) {
	res, err := runSQL(ctx, exec, c, dbName, sql, false)
	if err != nil {
		return "", err
	}
	return dmlMessage(c.Engine, res.Stdout), nil
}

func whereClause(engine string, pk map[string]*string) string {
	var parts []string
	for _, k := range sortedKeys(pk) {
		v := pk[k]
		if v == nil {
			parts = append(parts, quoteIdent(engine, k)+" IS NULL")
		} else {
			parts = append(parts, quoteIdent(engine, k)+" = "+sqlVal(engine, v))
		}
	}
	return strings.Join(parts, " AND ")
}

// ───────── quoting helpers ─────────

// qualIdent builds a fully-quoted table reference. For mysql the connection is
// already scoped to the database (-D), so the bare table name is enough.
func qualIdent(engine, schema, table string) string {
	if engine == "postgres" {
		return quoteIdent(engine, schema) + "." + quoteIdent(engine, table)
	}
	return quoteIdent(engine, table)
}

func quoteIdent(engine, s string) string {
	if engine == "postgres" {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// sqlLit single-quotes a string for use as a SQL literal.
func sqlLit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// sqlVal renders a value as a quoted literal. Backslash handling differs per
// engine: mysql treats \ as an escape inside literals (NO_BACKSLASH_ESCAPES is
// off by default) so it must be doubled; postgres (standard_conforming_strings
// on since 9.1) treats it literally so it must NOT be.
func sqlVal(engine string, v *string) string {
	if v == nil {
		return "NULL"
	}
	s := *v
	if engine == "mysql" {
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func firstColumn(qr *QueryResult) []string {
	out := []string{}
	for _, row := range qr.Rows {
		if v, ok := cellString(row, 0); ok {
			out = append(out, v)
		}
	}
	return out
}

// cellString extracts a non-NULL string cell from a result row.
func cellString(row []any, i int) (string, bool) {
	if i >= len(row) || row[i] == nil {
		return "", false
	}
	s, ok := row[i].(string)
	return s, ok
}

func sortedKeys(m map[string]*string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// validContainerID matches docker IDs / names; defense against shell injection
// since the ID is interpolated unquoted.
func validContainerID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-' || r == '/':
		default:
			return false
		}
		if i == 0 && (r == '.' || r == '-') {
			return false
		}
	}
	return true
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
