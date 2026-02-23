// query-api: HTTP service for querying telemetry from QuestDB via PostgreSQL wire protocol.
//
// Routes:
//   GET  /healthz       → public health check (DB ping)
//   POST /v1/query      → execute parameterized SQL (org_id auto-injected)
//   GET  /v1/telemetry  → convenience telemetry query with downsampling support
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/systemscale/services/shared/auth"
)

type config struct {
	HTTPAddr   string
	QuestDBDSN string
	JWKSURLs   string
}

func loadConfig() config {
	jwks := os.Getenv("JWKS_URLS")
	if jwks == "" {
		jwks = os.Getenv("JWKS_URL")
	}
	return config{
		HTTPAddr:   envOr("HTTP_ADDR", ":8081"),
		QuestDBDSN: envOr("QUESTDB_DSN", "user=admin password=quest host=localhost port=8812 dbname=qdb sslmode=disable"),
		JWKSURLs:   jwks,
	}
}

type server struct {
	db        *sql.DB
	validator *auth.Validator
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg := loadConfig()
	slog.Info("starting query-api", "addr", cfg.HTTPAddr)

	db, err := sql.Open("postgres", cfg.QuestDBDSN)
	if err != nil {
		slog.Error("QuestDB open", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		slog.Error("QuestDB ping", "err", err)
		os.Exit(1)
	}
	slog.Info("QuestDB connected")

	var validator *auth.Validator
	if cfg.JWKSURLs != "" {
		validator, err = auth.NewValidator(cfg.JWKSURLs)
		if err != nil {
			slog.Error("auth validator init", "err", err)
			os.Exit(1)
		}
		slog.Info("auth validator initialized")
	} else {
		slog.Warn("JWKS_URLS not set — auth middleware will reject all requests")
	}

	srv := &server{db: db, validator: validator}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	httpServer := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      srv.routes(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down query-api")
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		httpServer.Shutdown(shutCtx)
	}()

	slog.Info("query-api ready", "addr", cfg.HTTPAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP serve", "err", err)
		os.Exit(1)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Routes
// ──────────────────────────────────────────────────────────────────────────────

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", s.handleHealthz)

	if s.validator != nil {
		protect := auth.HTTPMiddleware(s.validator)
		mux.Handle("/v1/query", protect(http.HandlerFunc(s.handleQuery)))
		mux.Handle("/v1/telemetry", protect(http.HandlerFunc(s.handleTelemetry)))
	}

	return auth.RequestIDMiddleware(corsMiddleware(mux))
}

// ──────────────────────────────────────────────────────────────────────────────
// Handlers
// ──────────────────────────────────────────────────────────────────────────────

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		http.Error(w, "db unreachable", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprint(w, "ok")
}

func (s *server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		SQL    string        `json:"sql"`
		Params []interface{} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.SQL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sql is required"})
		return
	}

	trimmed := strings.TrimSpace(req.SQL)
	if !strings.HasPrefix(strings.ToUpper(trimmed), "SELECT") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only SELECT queries are allowed"})
		return
	}

	query, queryArgs := injectOrgFilter(trimmed, claims.OrgID)

	rows, err := s.db.QueryContext(r.Context(), query, queryArgs...)
	if err != nil {
		slog.Error("query execution failed", "err", err, "sql", query)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query execution failed"})
		return
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read columns"})
		return
	}

	var result [][]interface{}
	for rows.Next() {
		values := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			slog.Warn("row scan error", "err", err)
			continue
		}
		row := make([]interface{}, len(cols))
		for i, v := range values {
			switch val := v.(type) {
			case []byte:
				row[i] = string(val)
			case time.Time:
				row[i] = val.Format(time.RFC3339Nano)
			default:
				row[i] = val
			}
		}
		result = append(result, row)
	}
	if result == nil {
		result = [][]interface{}{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"columns": cols,
		"rows":    result,
		"count":   len(result),
	})
}

var validInterval = regexp.MustCompile(`^\d+[smhd]$`)

func (s *server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	q := r.URL.Query()

	vehicleID := q.Get("vehicle_id")
	if vehicleID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "vehicle_id is required"})
		return
	}

	streamType := q.Get("stream_type")
	if streamType == "" {
		streamType = "telemetry"
	}

	startExpr := q.Get("start")
	if startExpr == "" {
		startExpr = "-1h"
	}
	endExpr := q.Get("end")

	sampleBy := q.Get("sample_by")
	if sampleBy != "" && !validInterval.MatchString(sampleBy) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid sample_by format (e.g. 1m, 5s, 1h)"})
		return
	}

	limit := 1000
	if ls := q.Get("limit"); ls != "" {
		if l, err := strconv.Atoi(ls); err == nil && l > 0 {
			limit = l
		}
	}
	if limit > 10000 {
		limit = 10000
	}

	projectID := q.Get("project")

	var columns string
	if sampleBy != "" {
		columns = `timestamp,
			first(vehicle_id) as vehicle_id, first(stream_type) as stream_type,
			first(stream_name) as stream_name,
			avg(lat) as lat, avg(lon) as lon, avg(alt_m) as alt_m,
			last(seq) as seq, last(payload_json) as payload_json,
			avg(battery_pct) as battery_pct, avg(speed_ms) as speed_ms, avg(heading) as heading`
	} else {
		columns = `timestamp, vehicle_id, stream_type, stream_name,
			lat, lon, alt_m, seq, payload_json,
			battery_pct, speed_ms, heading`
	}

	// Build query with numbered placeholders for user-supplied values.
	// QuestDB supports $1-style parameters over the PostgreSQL wire protocol.
	args := []interface{}{vehicleID, claims.OrgID, streamType}
	query := fmt.Sprintf("SELECT %s FROM telemetry WHERE vehicle_id = $1 AND org_id = $2 AND stream_type = $3", columns)

	nextParam := 4

	if timeSQL := parseTimeExpr(startExpr); timeSQL != "" {
		query += " AND timestamp > " + timeSQL
	}
	if endExpr != "" {
		if endSQL := parseTimeExpr(endExpr); endSQL != "" {
			query += " AND timestamp < " + endSQL
		}
	}

	if projectID != "" {
		query += fmt.Sprintf(" AND project_id = $%d", nextParam)
		args = append(args, projectID)
		nextParam++
	}

	if sampleBy != "" {
		query += " SAMPLE BY " + sampleBy
	}

	query += " ORDER BY timestamp DESC"
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		slog.Error("telemetry query failed", "err", err, "sql", query)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var results []map[string]interface{}

	for rows.Next() {
		values := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		row := make(map[string]interface{}, len(cols))
		for i, col := range cols {
			switch val := values[i].(type) {
			case []byte:
				row[col] = string(val)
			case time.Time:
				row[col] = val.Format(time.RFC3339Nano)
			default:
				row[col] = val
			}
		}
		results = append(results, row)
	}
	if results == nil {
		results = []map[string]interface{}{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"rows":  results,
		"count": len(results),
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// SQL helpers
// ──────────────────────────────────────────────────────────────────────────────

// injectOrgFilter appends an org_id filter to queries on the telemetry table
// to prevent cross-organization data access. Uses a parameterized placeholder
// so the value is never interpolated into the SQL string.
func injectOrgFilter(query, orgID string) (string, []interface{}) {
	lower := strings.ToLower(query)
	if !strings.Contains(lower, "telemetry") {
		return query, nil
	}
	if strings.Contains(lower, "org_id") {
		return query, nil
	}

	insertPos := findClauseInsertPos(lower)

	if strings.Contains(lower, "where") {
		return query[:insertPos] + " AND org_id = $1" + " " + query[insertPos:], []interface{}{orgID}
	}
	return query[:insertPos] + " WHERE org_id = $1" + " " + query[insertPos:], []interface{}{orgID}
}

// findClauseInsertPos returns the position before ORDER BY, GROUP BY, LIMIT,
// or SAMPLE BY — the safe place to inject a WHERE/AND condition.
func findClauseInsertPos(lower string) int {
	keywords := []string{" order by", " group by", " limit ", " sample by"}
	pos := len(lower)
	for _, kw := range keywords {
		idx := strings.Index(lower, kw)
		if idx != -1 && idx < pos {
			pos = idx
		}
	}
	return pos
}

// parseTimeExpr converts a relative time expression ("-1h", "-30m") into a
// QuestDB dateadd() call, or wraps an absolute ISO timestamp in quotes.
func parseTimeExpr(s string) string {
	if s == "" {
		return ""
	}
	if len(s) >= 3 && s[0] == '-' {
		unit := s[len(s)-1]
		numStr := s[1 : len(s)-1]
		if n, err := strconv.Atoi(numStr); err == nil {
			var qdbUnit string
			switch unit {
			case 's':
				qdbUnit = "s"
			case 'm':
				qdbUnit = "m"
			case 'h':
				qdbUnit = "h"
			case 'd':
				qdbUnit = "d"
			}
			if qdbUnit != "" {
				return fmt.Sprintf("dateadd('%s', -%d, now())", qdbUnit, n)
			}
		}
	}
	return fmt.Sprintf("'%s'", escapeSQLString(s))
}

func escapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// ──────────────────────────────────────────────────────────────────────────────
// Utility
// ──────────────────────────────────────────────────────────────────────────────

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := os.Getenv("CORS_ALLOW_ORIGIN")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
