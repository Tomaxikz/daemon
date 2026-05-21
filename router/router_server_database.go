package router

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	_ "github.com/glebarez/go-sqlite"
	"github.com/pterodactyl/wings/router/middleware"
)

const (
	betterFilesDatabaseMaxFile         = 256 * 1024 * 1024
	betterFilesDatabaseDefaultPageSize = 25
	betterFilesDatabaseMaxPageSize     = 100
	betterFilesDatabaseMaxSearchCols   = 16
	betterFilesDatabaseMaxSearchLength = 200
)

type betterFilesDatabaseTableSummary struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type betterFilesDatabaseColumn struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Nullable   bool   `json:"nullable"`
	Default    any    `json:"default"`
	PrimaryKey bool   `json:"primary_key"`
}

type betterFilesDatabaseIndex struct {
	Name   string `json:"name"`
	Unique bool   `json:"unique"`
	Origin string `json:"origin"`
}

type betterFilesDatabasePagination struct {
	Total       int `json:"total"`
	PerPage     int `json:"per_page"`
	CurrentPage int `json:"current_page"`
	TotalPages  int `json:"total_pages"`
}

type betterFilesDatabaseInitializeRequest struct {
	File string `json:"file"`
}

type betterFilesDatabaseCreateTableRequest struct {
	File    string                                  `json:"file"`
	Name    string                                  `json:"name"`
	Columns []betterFilesDatabaseCreateTableColumn `json:"columns"`
}

type betterFilesDatabaseCreateTableColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type betterFilesDatabaseUpdateCellRequest struct {
	File   string `json:"file"`
	Table  string `json:"table"`
	RowID  int64  `json:"row_id"`
	Column string `json:"column"`
	Value  any    `json:"value"`
}

type betterFilesDatabaseInsertRowRequest struct {
	File   string         `json:"file"`
	Table  string         `json:"table"`
	Values map[string]any `json:"values"`
}

type betterFilesDatabaseDeleteRowRequest struct {
	File  string `json:"file"`
	Table string `json:"table"`
	RowID int64  `json:"row_id"`
}

func getServerDatabaseInspect(c *gin.Context) {
	s := middleware.ExtractServer(c)
	db, displayPath, cleanup, ok := betterFilesOpenServerDatabase(c, s.Filesystem().Path(), c.Query("file"))
	if !ok {
		return
	}
	defer cleanup()

	tables, err := betterFilesDatabaseTables(db)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"file": displayPath,
		"tables": tables,
	})
}

func getServerDatabaseTable(c *gin.Context) {
	s := middleware.ExtractServer(c)
	db, displayPath, cleanup, ok := betterFilesOpenServerDatabase(c, s.Filesystem().Path(), c.Query("file"))
	if !ok {
		return
	}
	defer cleanup()

	tableName := strings.TrimSpace(c.Query("table"))
	if tableName == "" || len(tableName) > 255 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid database table."})
		return
	}

	page := betterFilesDatabaseBoundedInt(c.Query("page"), 1, 1, 100000)
	perPage := betterFilesDatabaseBoundedInt(c.Query("per_page"), betterFilesDatabaseDefaultPageSize, 1, betterFilesDatabaseMaxPageSize)
	search := strings.TrimSpace(c.Query("search"))
	if len(search) > betterFilesDatabaseMaxSearchLength {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Database search query is too long."})
		return
	}

	tables, err := betterFilesDatabaseTables(db)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tableSummary, ok := betterFilesDatabaseFindTable(tables, tableName)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Database table was not found."})
		return
	}
	editable := tableSummary.Type == "table" && betterFilesDatabaseTableHasRowID(db, tableName)

	columns, err := betterFilesDatabaseColumns(db, tableName)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	indexes, err := betterFilesDatabaseIndexes(db, tableName)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	where, args := betterFilesDatabaseSearchWhere(columns, search)
	total, err := betterFilesDatabaseCountRows(db, tableName, where, args)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	rows, err := betterFilesDatabaseRows(db, tableName, where, args, perPage, (page-1)*perPage, editable)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	totalPages := 1
	if total > 0 {
		totalPages = (total + perPage - 1) / perPage
	}

	c.JSON(http.StatusOK, gin.H{
		"file":    displayPath,
		"table":   tableName,
		"columns": columns,
		"indexes": indexes,
		"editable": editable,
		"rows":    rows,
		"pagination": betterFilesDatabasePagination{
			Total:       total,
			PerPage:     perPage,
			CurrentPage: page,
			TotalPages:  totalPages,
		},
		"search": search,
	})
}

func postServerDatabaseInitialize(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var request betterFilesDatabaseInitializeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid database initialization request."})
		return
	}

	hostPath, displayPath, ok := betterFilesResolveServerFile(s.Filesystem().Path(), request.File, true)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid database path."})
		return
	}
	if !betterFilesIsDatabaseFile(displayPath) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Only .db, .sqlite, and .sqlite3 files can be initialized."})
		return
	}

	stat, err := os.Stat(hostPath)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	if stat.IsDir() || stat.Size() != 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Only empty database files can be initialized."})
		return
	}

	db, err := betterFilesOpenSQLiteWritable(hostPath)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer db.Close()

	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Unable to initialize SQLite database: %s", err.Error())})
		return
	}

	c.JSON(http.StatusOK, gin.H{"file": displayPath})
}

func postServerDatabaseTable(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var request betterFilesDatabaseCreateTableRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid database table create request."})
		return
	}

	db, cleanup, ok := betterFilesOpenServerDatabaseWritable(c, s.Filesystem().Path(), request.File)
	if !ok {
		return
	}
	defer cleanup()

	if err := betterFilesDatabaseCreateTable(db, request.Name, request.Columns); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"table": strings.TrimSpace(request.Name)})
}

func patchServerDatabaseCell(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var request betterFilesDatabaseUpdateCellRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid database cell update request."})
		return
	}

	db, cleanup, ok := betterFilesOpenServerDatabaseWritable(c, s.Filesystem().Path(), request.File)
	if !ok {
		return
	}
	defer cleanup()

	if err := betterFilesDatabaseUpdateCell(db, request.Table, request.RowID, request.Column, request.Value); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"updated": true})
}

func postServerDatabaseRow(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var request betterFilesDatabaseInsertRowRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid database row insert request."})
		return
	}

	db, cleanup, ok := betterFilesOpenServerDatabaseWritable(c, s.Filesystem().Path(), request.File)
	if !ok {
		return
	}
	defer cleanup()

	rowID, err := betterFilesDatabaseInsertRow(db, request.Table, request.Values)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"row_id": rowID})
}

func deleteServerDatabaseRow(c *gin.Context) {
	s := middleware.ExtractServer(c)
	var request betterFilesDatabaseDeleteRowRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid database row delete request."})
		return
	}

	db, cleanup, ok := betterFilesOpenServerDatabaseWritable(c, s.Filesystem().Path(), request.File)
	if !ok {
		return
	}
	defer cleanup()

	if err := betterFilesDatabaseDeleteRow(db, request.Table, request.RowID); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

func betterFilesOpenServerDatabase(c *gin.Context, root string, rawFile string) (*sql.DB, string, func(), bool) {
	hostPath, displayPath, ok := betterFilesResolveServerFile(root, rawFile, true)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid database path."})
		return nil, "", func() {}, false
	}
	if !betterFilesIsDatabaseFile(displayPath) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Only .db, .sqlite, and .sqlite3 files can be viewed as databases."})
		return nil, "", func() {}, false
	}

	stat, err := os.Stat(hostPath)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return nil, "", func() {}, false
	}
	if stat.IsDir() || stat.Size() <= 0 || stat.Size() > betterFilesDatabaseMaxFile {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Database is empty, too large, or is not a file."})
		return nil, "", func() {}, false
	}

	db, err := betterFilesOpenSQLiteReadOnly(hostPath)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return nil, "", func() {}, false
	}

	return db, displayPath, func() {
		_ = db.Close()
	}, true
}

func betterFilesOpenServerDatabaseWritable(c *gin.Context, root string, rawFile string) (*sql.DB, func(), bool) {
	hostPath, displayPath, ok := betterFilesResolveServerFile(root, rawFile, true)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid database path."})
		return nil, func() {}, false
	}
	if !betterFilesIsDatabaseFile(displayPath) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Only .db, .sqlite, and .sqlite3 files can be edited as databases."})
		return nil, func() {}, false
	}

	stat, err := os.Stat(hostPath)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return nil, func() {}, false
	}
	if stat.IsDir() || stat.Size() <= 0 || stat.Size() > betterFilesDatabaseMaxFile {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Database is empty, too large, or is not a file."})
		return nil, func() {}, false
	}

	db, err := betterFilesOpenSQLiteWritable(hostPath)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return nil, func() {}, false
	}

	return db, func() {
		_ = db.Close()
	}, true
}

func betterFilesOpenSQLiteReadOnly(hostPath string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: hostPath}
	values := u.Query()
	values.Set("mode", "ro")
	u.RawQuery = values.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("Unable to open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec("PRAGMA query_only = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("Unable to enable read-only SQLite mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 250"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("Unable to configure SQLite timeout: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("Unable to read SQLite database: %w", err)
	}

	return db, nil
}

func betterFilesOpenSQLiteWritable(hostPath string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: hostPath}
	values := u.Query()
	values.Set("mode", "rwc")
	u.RawQuery = values.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("Unable to open SQLite database for writing: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec("PRAGMA busy_timeout = 250"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("Unable to configure SQLite timeout: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("Unable to write SQLite database: %w", err)
	}

	return db, nil
}

func betterFilesDatabaseTables(db *sql.DB) ([]betterFilesDatabaseTableSummary, error) {
	rows, err := db.Query("SELECT name, type FROM sqlite_master WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%' ORDER BY lower(name)")
	if err != nil {
		return nil, fmt.Errorf("Unable to list database tables: %w", err)
	}
	defer rows.Close()

	tables := make([]betterFilesDatabaseTableSummary, 0, 16)
	for rows.Next() {
		var table betterFilesDatabaseTableSummary
		if err := rows.Scan(&table.Name, &table.Type); err != nil {
			return nil, fmt.Errorf("Unable to read database table metadata: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("Unable to read database tables: %w", err)
	}

	return tables, nil
}

func betterFilesDatabaseFindTable(tables []betterFilesDatabaseTableSummary, name string) (betterFilesDatabaseTableSummary, bool) {
	for _, table := range tables {
		if table.Name == name {
			return table, true
		}
	}

	return betterFilesDatabaseTableSummary{}, false
}

func betterFilesDatabaseTableHasRowID(db *sql.DB, table string) bool {
	var schema sql.NullString
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&schema); err != nil {
		return false
	}
	if !schema.Valid {
		return false
	}

	return !strings.Contains(strings.ToUpper(schema.String), "WITHOUT ROWID")
}

func betterFilesDatabaseColumns(db *sql.DB, table string) ([]betterFilesDatabaseColumn, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", betterFilesDatabaseIdent(table)))
	if err != nil {
		return nil, fmt.Errorf("Unable to read database columns: %w", err)
	}
	defer rows.Close()

	columns := make([]betterFilesDatabaseColumn, 0, 16)
	for rows.Next() {
		var cid int
		var name string
		var columnType sql.NullString
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int

		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("Unable to read database column metadata: %w", err)
		}

		var normalizedDefault any
		if defaultValue.Valid {
			normalizedDefault = defaultValue.String
		}

		columns = append(columns, betterFilesDatabaseColumn{
			Name:       name,
			Type:       strings.ToUpper(columnType.String),
			Nullable:   notNull == 0,
			Default:    normalizedDefault,
			PrimaryKey: primaryKey > 0,
		})
		_ = cid
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("Unable to read database columns: %w", err)
	}

	return columns, nil
}

func betterFilesDatabaseIndexes(db *sql.DB, table string) ([]betterFilesDatabaseIndex, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA index_list(%s)", betterFilesDatabaseIdent(table)))
	if err != nil {
		return nil, fmt.Errorf("Unable to read database indexes: %w", err)
	}
	defer rows.Close()

	indexes := make([]betterFilesDatabaseIndex, 0, 8)
	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin sql.NullString
		var partial int

		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return nil, fmt.Errorf("Unable to read database index metadata: %w", err)
		}

		indexes = append(indexes, betterFilesDatabaseIndex{
			Name:   name,
			Unique: unique == 1,
			Origin: origin.String,
		})
		_, _ = seq, partial
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("Unable to read database indexes: %w", err)
	}

	sort.SliceStable(indexes, func(i, j int) bool {
		return strings.ToLower(indexes[i].Name) < strings.ToLower(indexes[j].Name)
	})

	return indexes, nil
}

func betterFilesDatabaseCountRows(db *sql.DB, table string, where string, args []any) (int, error) {
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s%s", betterFilesDatabaseIdent(table), where)
	var total int
	if err := db.QueryRow(query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("Unable to count database rows: %w", err)
	}

	return total, nil
}

func betterFilesDatabaseRows(db *sql.DB, table string, where string, args []any, limit int, offset int, includeRowID bool) ([]map[string]any, error) {
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, limit, offset)

	selectClause := "*"
	if includeRowID {
		selectClause = "rowid AS __betterfiles_rowid__, *"
	}
	query := fmt.Sprintf("SELECT %s FROM %s%s LIMIT ? OFFSET ?", selectClause, betterFilesDatabaseIdent(table), where)
	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("Unable to read database rows: %w", err)
	}
	defer rows.Close()

	names, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("Unable to read database result columns: %w", err)
	}

	results := make([]map[string]any, 0, limit)
	values := make([]any, len(names))
	scanTargets := make([]any, len(names))
	for i := range values {
		scanTargets[i] = &values[i]
	}

	for rows.Next() {
		for i := range values {
			values[i] = nil
		}
		if err := rows.Scan(scanTargets...); err != nil {
			return nil, fmt.Errorf("Unable to scan database row: %w", err)
		}

		row := make(map[string]any, len(names))
		for i, name := range names {
			row[name] = betterFilesDatabaseValue(values[i])
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("Unable to read database rows: %w", err)
	}

	return results, nil
}

func betterFilesDatabaseCreateTable(db *sql.DB, table string, columns []betterFilesDatabaseCreateTableColumn) error {
	table = strings.TrimSpace(table)
	if !betterFilesDatabaseValidName(table) {
		return fmt.Errorf("Invalid database table name.")
	}
	if len(columns) == 0 || len(columns) > 64 {
		return fmt.Errorf("A database table needs between 1 and 64 columns.")
	}

	seen := make(map[string]struct{}, len(columns))
	definitions := make([]string, 0, len(columns))
	for _, column := range columns {
		name := strings.TrimSpace(column.Name)
		if !betterFilesDatabaseValidName(name) {
			return fmt.Errorf("Invalid database column name.")
		}
		if _, ok := seen[strings.ToLower(name)]; ok {
			return fmt.Errorf("Duplicate database column name: %s", name)
		}
		seen[strings.ToLower(name)] = struct{}{}

		columnType, ok := betterFilesDatabaseColumnType(column.Type)
		if !ok {
			return fmt.Errorf("Invalid database column type for %s.", name)
		}

		definitions = append(definitions, fmt.Sprintf("%s %s", betterFilesDatabaseIdent(name), columnType))
	}

	query := fmt.Sprintf("CREATE TABLE %s (%s)", betterFilesDatabaseIdent(table), strings.Join(definitions, ", "))
	if _, err := db.Exec(query); err != nil {
		return fmt.Errorf("Unable to create database table: %w", err)
	}

	return nil
}

func betterFilesDatabaseUpdateCell(db *sql.DB, table string, rowID int64, column string, value any) error {
	if rowID <= 0 {
		return fmt.Errorf("Invalid database row.")
	}
	columns, err := betterFilesDatabaseWritableColumns(db, table)
	if err != nil {
		return err
	}
	if _, ok := columns[column]; !ok {
		return fmt.Errorf("Database column was not found.")
	}

	result, err := db.Exec(
		fmt.Sprintf("UPDATE %s SET %s = ? WHERE rowid = ?", betterFilesDatabaseIdent(table), betterFilesDatabaseIdent(column)),
		betterFilesDatabaseWriteValue(value),
		rowID,
	)
	if err != nil {
		return fmt.Errorf("Unable to update database cell: %w", err)
	}

	return betterFilesDatabaseRequireAffected(result, "Database row was not found.")
}

func betterFilesDatabaseInsertRow(db *sql.DB, table string, values map[string]any) (int64, error) {
	columns, err := betterFilesDatabaseWritableColumns(db, table)
	if err != nil {
		return 0, err
	}

	names := make([]string, 0, len(values))
	for name := range values {
		if _, ok := columns[name]; !ok {
			return 0, fmt.Errorf("Database column was not found: %s", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	args := make([]any, 0, len(names))
	idents := make([]string, 0, len(names))
	placeholders := make([]string, 0, len(names))
	for _, name := range names {
		idents = append(idents, betterFilesDatabaseIdent(name))
		placeholders = append(placeholders, "?")
		args = append(args, betterFilesDatabaseWriteValue(values[name]))
	}

	query := fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", betterFilesDatabaseIdent(table))
	if len(names) > 0 {
		query = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", betterFilesDatabaseIdent(table), strings.Join(idents, ", "), strings.Join(placeholders, ", "))
	}

	result, err := db.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("Unable to insert database row: %w", err)
	}

	rowID, err := result.LastInsertId()
	if err != nil {
		return 0, nil
	}

	return rowID, nil
}

func betterFilesDatabaseDeleteRow(db *sql.DB, table string, rowID int64) error {
	if rowID <= 0 {
		return fmt.Errorf("Invalid database row.")
	}
	if _, err := betterFilesDatabaseWritableColumns(db, table); err != nil {
		return err
	}

	result, err := db.Exec(fmt.Sprintf("DELETE FROM %s WHERE rowid = ?", betterFilesDatabaseIdent(table)), rowID)
	if err != nil {
		return fmt.Errorf("Unable to delete database row: %w", err)
	}

	return betterFilesDatabaseRequireAffected(result, "Database row was not found.")
}

func betterFilesDatabaseWritableColumns(db *sql.DB, table string) (map[string]betterFilesDatabaseColumn, error) {
	tables, err := betterFilesDatabaseTables(db)
	if err != nil {
		return nil, err
	}
	tableSummary, ok := betterFilesDatabaseFindTable(tables, table)
	if !ok {
		return nil, fmt.Errorf("Database table was not found.")
	}
	if tableSummary.Type != "table" {
		return nil, fmt.Errorf("Database views cannot be edited.")
	}
	if !betterFilesDatabaseTableHasRowID(db, table) {
		return nil, fmt.Errorf("This table cannot be edited because it does not expose SQLite row IDs.")
	}

	columns, err := betterFilesDatabaseColumns(db, table)
	if err != nil {
		return nil, err
	}
	result := make(map[string]betterFilesDatabaseColumn, len(columns))
	for _, column := range columns {
		result[column.Name] = column
	}

	return result, nil
}

func betterFilesDatabaseRequireAffected(result sql.Result, message string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return nil
	}
	if affected == 0 {
		return fmt.Errorf("%s", message)
	}

	return nil
}

func betterFilesDatabaseWriteValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return typed
	case float64:
		return typed
	case bool:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func betterFilesDatabaseSearchWhere(columns []betterFilesDatabaseColumn, search string) (string, []any) {
	if search == "" || len(columns) == 0 {
		return "", nil
	}

	limit := len(columns)
	if limit > betterFilesDatabaseMaxSearchCols {
		limit = betterFilesDatabaseMaxSearchCols
	}

	parts := make([]string, 0, limit)
	args := make([]any, 0, limit)
	needle := "%" + betterFilesDatabaseEscapeLike(search) + "%"
	for _, column := range columns[:limit] {
		parts = append(parts, fmt.Sprintf("CAST(%s AS TEXT) LIKE ? ESCAPE '\\'", betterFilesDatabaseIdent(column.Name)))
		args = append(args, needle)
	}

	return " WHERE (" + strings.Join(parts, " OR ") + ")", args
}

func betterFilesDatabaseValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case []byte:
		if betterFilesDatabaseLooksBinary(typed) {
			return fmt.Sprintf("[BLOB %d bytes]", len(typed))
		}
		return string(typed)
	default:
		return typed
	}
}

func betterFilesDatabaseLooksBinary(value []byte) bool {
	if len(value) == 0 {
		return false
	}

	limit := len(value)
	if limit > 512 {
		limit = 512
	}
	for _, b := range value[:limit] {
		if b == 0 || (b < 32 && b != '\n' && b != '\r' && b != '\t') {
			return true
		}
	}

	return false
}

func betterFilesDatabaseBoundedInt(raw string, fallback int, minimum int, maximum int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}

	return value
}

func betterFilesDatabaseIdent(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func betterFilesDatabaseEscapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

func betterFilesDatabaseValidName(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' {
			continue
		}
		return false
	}

	return true
}

func betterFilesDatabaseColumnType(value string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "TEXT":
		return "TEXT", true
	case "INTEGER":
		return "INTEGER", true
	case "INTEGER PRIMARY KEY":
		return "INTEGER PRIMARY KEY", true
	case "REAL":
		return "REAL", true
	case "BLOB":
		return "BLOB", true
	case "NUMERIC":
		return "NUMERIC", true
	default:
		return "", false
	}
}

func betterFilesIsDatabaseFile(value string) bool {
	switch strings.ToLower(path.Ext(value)) {
	case ".db", ".sqlite", ".sqlite3":
		return true
	default:
		return false
	}
}
