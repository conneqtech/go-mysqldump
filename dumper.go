package mysqldump

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/sirupsen/logrus"
	"io"
	"sync"
	"time"

	binary "github.com/MouseHatGames/go-mysqldump/internal/marshal"
)

const version = "1.0.0"

var comma = []byte{','}
var commaNewline = []byte{',', '\n', '\t'}
var quote = []byte{'\''}
var semicolonNewline = []byte{';', '\n'}

// filteredTables special queries for data dump. not should start with a space
var filteredTables = map[string]map[string][]string{
	"iot-api": {
		"event_log": {
			" WHERE event = 'geofence-in' AND id < 517837446",
			" WHERE event = 'geofence-out' AND id < 517837446",
			" WHERE id >= 517837446",
		}, // skip data before 01-10-2021 except for geofences
		"rate_limit_request_log": {}, // skip all data
	},
}

// Dumper represents a database.
type Dumper struct {
	db        *sql.DB
	w         io.Writer
	bin       *binary.Writer
	chunkSize int
}

// NewDumper creates a new dumper instance.
func NewDumper(db *sql.DB, w io.Writer, chunkSize int) *Dumper {
	return &Dumper{
		db:        db,
		w:         w,
		bin:       binary.NewWriter(w),
		chunkSize: chunkSize,
	}
}

// Dump dumps one or more tables from a database into a writer.
// If dbName is not empty, a "USE xxx" command will be sent prior to commencing the dump.
func (d *Dumper) Dump(dbName string, wg *sync.WaitGroup, tables ...string) error {
	var err error

	if len(tables) == 0 {
		return nil
	}

	// Get server version
	serverVer, err := getServerVersion(d.db)
	if err != nil {
		return err
	}

	if err = d.use(dbName); err != nil {
		return err
	}

	d.bin.WriteFileHeader(&binary.FileHeader{
		ServerVersion: serverVer,
		DatabaseName:  dbName,
		DumpStart:     time.Now().UTC(),
	})

	// Write sql for each table
	for _, t := range tables {
		if err := d.writeTable(t, dbName, wg); err != nil {
			return err
		}
	}

	return nil
}

// DumpAllTables dumps all tables in a database into a writer
// If dbName is not empty, a "USE xxx" command will be sent prior to commencing the dump.
func (d *Dumper) DumpAllTables(dbName string, wg *sync.WaitGroup) error {
	if err := d.use(dbName); err != nil {
		return err
	}

	// List tables in the database
	tables, err := d.getTables(dbName)
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}

	return d.Dump(dbName, wg, tables...)
}

func (d *Dumper) use(db string) error {
	if db != "" {
		// Use the database
		if _, err := d.db.Exec("USE `" + db + "`"); err != nil {
			return fmt.Errorf("use database: %w", err)
		}
	}

	return nil
}

func (d *Dumper) getTables(dbName string) ([]string, error) {
	tables := make([]string, 0)

	// Get table list
	rows, err := d.db.Query("SHOW TABLES")
	if err != nil {
		return tables, err
	}
	defer rows.Close()

	// Read result
	for rows.Next() {
		var table sql.NullString
		if err := rows.Scan(&table); err != nil {
			return tables, err
		}
		tables = append(tables, table.String)
	}
	return tables, rows.Err()
}

func getServerVersion(db *sql.DB) (string, error) {
	var server_version sql.NullString
	if err := db.QueryRow("SELECT version()").Scan(&server_version); err != nil {
		return "", err
	}
	return server_version.String, nil
}

func (d *Dumper) writeTable(name string, schema string, wg *sync.WaitGroup) error {
	var err error

	sql, err := getTableSQL(d.db, name)
	if err != nil {
		return fmt.Errorf("get table SQL: %w", err)
	}

	cols, err := getTableColumns(d.db, name, schema)
	if err != nil {
		return fmt.Errorf("get table columns: %w", err)
	}

	d.bin.WriteTableHeader(&binary.TableHeader{
		Name:      name,
		CreateSQL: sql,
		Columns:   cols,
	})

	logrus.Infof("Read table information for %s", name)
	if err = d.writeTableValues(name, schema, wg); err != nil {
		return fmt.Errorf("write table rows: %w", err)
	}

	return nil
}

func getTableSQL(db *sql.DB, name string) (string, error) {
	// Get table creation SQL
	var table_return sql.NullString
	var table_sql sql.NullString
	err := db.QueryRow("SHOW CREATE TABLE "+name).Scan(&table_return, &table_sql)

	if err != nil {
		return "", err
	}
	if table_return.String != name {
		return "", errors.New("returned table is not the same as requested table")
	}

	return table_sql.String, nil
}

func getTableColumns(db *sql.DB, table string, schema string) (cols []string, err error) {
	rows, err := db.Query("SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_NAME = ? AND TABLE_SCHEMA = ?", table, schema)
	if err != nil {
		return nil, err
	}

	var column string
	for rows.Next() {
		err = rows.Scan(&column)
		if err != nil {
			return nil, err
		}

		cols = append(cols, column)
	}

	return
}

func (d *Dumper) writeTableValues(name string, schema string, wg *sync.WaitGroup) error {
	var queries = []string{""}
	if fs, ok := filteredTables[schema]; ok {
		if q, ok := fs[name]; ok {
			queries = q
		}
	}
	for _, filter := range queries {
		offset := 0

		for {
			gotData := false
			wg.Wait()
			// Get Data
			logrus.Infof("Reading row data for table %s, offset = %d", name, offset)
			var rows *sql.Rows
			var err error
			if d.chunkSize > 0 {
				logrus.Debugf("SELECT * FROM "+name+filter+" LIMIT ? OFFSET ?", d.chunkSize, offset)
				rows, err = d.db.Query("SELECT * FROM "+name+filter+" LIMIT ? OFFSET ?", d.chunkSize, offset)
			} else {
				logrus.Debugf("SELECT * FROM " + name + filter)
				rows, err = d.db.Query("SELECT * FROM " + name + filter)
			}
			if err != nil {
				return err
			}

			// Get columns
			columns, err := rows.Columns()
			if err != nil {
				rows.Close()
				return err
			}
			if len(columns) == 0 {
				rows.Close()
				return errors.New("no columns in table " + name + ".")
			}

			for rows.Next() {
				gotData = true
				if err = d.writeValues(rows, columns); err != nil {
					rows.Close()
					return fmt.Errorf("write values: %w", err)
				}
			}

			rows.Close()

			if !gotData || d.chunkSize <= 0 {
				break
			}
			offset += d.chunkSize
			logrus.Infof("Wrote row for table %s, next offset = %d", name, offset)
		}
	}

	return nil
}

func (d *Dumper) writeValues(rows *sql.Rows, columns []string) error {
	data := make([]*string, len(columns))
	ptrs := make([]interface{}, len(columns))
	for i := range data {
		ptrs[i] = &data[i]
	}

	// Read data
	if err := rows.Scan(ptrs...); err != nil {
		return err
	}

	return d.bin.WriteRowData(data)
}
