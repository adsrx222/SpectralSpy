package livedemo

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

func init() {
	sql.Register("d1-proxy", &D1Driver{})
}

type D1Driver struct{}

type d1Tx struct{}

type d1Conn struct {
	endpoint string
	token    string
	client   *http.Client
}

type d1Stmt struct {
	conn  *d1Conn
	query string
}

type d1Rows struct {
	results []map[string]interface{}
	cols    []string
	index   int
}

type d1ResponseEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result []struct {
		Results []map[string]interface{} `json:"results"`
		Success bool                     `json:"success"`
	} `json:"result"`
}

func (d *D1Driver) Open(name string) (driver.Conn, error) {
	u, err := url.Parse(name)
	if err != nil {
		return nil, fmt.Errorf("invalid D1 DSN: %w", err)
	}

	token := u.Query().Get("token")
	q := u.Query()
	q.Del("token")
	u.RawQuery = q.Encode()

	return &d1Conn{
		endpoint: u.String(),
		token:    token,
		client:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (c *d1Conn) Prepare(query string) (driver.Stmt, error) {
	return &d1Stmt{conn: c, query: query}, nil
}

func (c *d1Conn) Close() error {
	return nil
}

func (c *d1Conn) Begin() (driver.Tx, error) {
	return &d1Tx{}, nil
}

func (t *d1Tx) Commit() error   { return nil }

func (t *d1Tx) Rollback() error { return nil }

func (s *d1Stmt) Close() error {
	return nil
}

func (s *d1Stmt) NumInput() int {
	return -1
}

func (s *d1Stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.conn.ExecContext(context.Background(), s.query, valuesToNamed(args))
}

func (s *d1Stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.conn.QueryContext(context.Background(), s.query, valuesToNamed(args))
}

func (s *d1Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}

func (s *d1Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}

func (c *d1Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	_, err := c.executeQuery(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(0), nil
}

func (c *d1Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	results, err := c.executeQuery(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return newD1Rows(results), nil
}

func (c *d1Conn) executeQuery(ctx context.Context, query string, args []driver.NamedValue) ([]map[string]interface{}, error) {
	paramValues := make([]interface{}, len(args))
	for i, arg := range args {
		paramValues[i] = arg.Value
	}

	payload, err := json.Marshal(map[string]interface{}{
		"sql":    query,
		"params": paramValues,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal query payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewBuffer(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("D1 HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("D1 API error: status %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var env d1ResponseEnvelope
	if err := json.Unmarshal(bodyBytes, &env); err != nil {
		var directResults []map[string]interface{}
		if directErr := json.Unmarshal(bodyBytes, &directResults); directErr == nil {
			return directResults, nil
		}
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if !env.Success && len(env.Errors) > 0 {
		return nil, fmt.Errorf("D1 API error %d: %s", env.Errors[0].Code, env.Errors[0].Message)
	}

	var results []map[string]interface{}
	if len(env.Result) > 0 {
		results = env.Result[0].Results
	}

	return results, nil
}

func (r *d1Rows) Columns() []string {
	return r.cols
}

func (r *d1Rows) Close() error {
	return nil
}

func (r *d1Rows) Next(dest []driver.Value) error {
	if r.index >= len(r.results) {
		return io.EOF
	}

	row := r.results[r.index]
	r.index++

	for i, col := range r.cols {
		dest[i] = row[col]
	}

	return nil
}

func valuesToNamed(args []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return named
}

func newD1Rows(results []map[string]interface{}) *d1Rows {
	var cols []string
	if len(results) > 0 {
		for k := range results[0] {
			cols = append(cols, k)
		}
	}
	return &d1Rows{
		results: results,
		cols:    cols,
		index:   0,
	}
}